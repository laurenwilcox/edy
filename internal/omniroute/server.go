// Package omniroute — локальный прокси, который представляет любой источник ИИ
// как OpenAI-совместимый API на 127.0.0.1:11435.
package omniroute

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/laurenwilcox/name/internal/store"
)

const (
	DefaultAddr = "127.0.0.1:11435"

	// SettingDailyBudget — лимит расходов в долларах за календарные сутки (0 — без лимита).
	SettingDailyBudget = "daily_budget_usd"

	// ClientHeader — заголовок, которым агенты Edy подписывают свои запросы
	// для учёта расходов по агентам.
	ClientHeader = "X-Edy-Client"

	defaultCooldown = time.Minute
	maxBodyBytes    = 32 << 20
)

// Server — HTTP-обработчик OmniRoute.
type Server struct {
	store    *store.Store
	adapters map[string]Adapter
	token    string
	log      *slog.Logger
	now      func() time.Time

	mu        sync.Mutex
	cooldowns map[int64]time.Time // ключ → время, до которого он «отдыхает» после 429
}

// NewServer создаёт OmniRoute. token — Bearer-токен, без которого запросы
// не принимаются (к прокси могут обращаться и внешние программы).
func NewServer(st *store.Store, token string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		store: st,
		adapters: map[string]Adapter{
			store.KindOpenAI:    &OpenAIAdapter{},
			store.KindAnthropic: &AnthropicAdapter{},
			store.KindCLI:       &CLIAdapter{},
		},
		token:     token,
		log:       log,
		now:       time.Now,
		cooldowns: map[int64]time.Time{},
	}
}

// RegisterAdapter подключает адаптер для типа провайдера (например, веб-чаты,
// которым нужен общий менеджер браузеров).
func (s *Server) RegisterAdapter(kind string, a Adapter) {
	s.adapters[kind] = a
}

// NewToken генерирует случайный токен доступа.
func NewToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "edy-" + hex.EncodeToString(b), nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.Write([]byte("ok"))
		return
	}
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "authentication_error", "нужен заголовок Authorization: Bearer <токен OmniRoute>")
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		s.handleModels(w)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		s.handleChat(w, r)
	default:
		writeError(w, http.StatusNotFound, "not_found", "неизвестный маршрут "+r.Method+" "+r.URL.Path)
	}
}

func (s *Server) authorized(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

func (s *Server) handleModels(w http.ResponseWriter) {
	providers, err := s.store.Providers()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	list := []model{}
	for _, p := range providers {
		for _, m := range p.Models {
			list = append(list, model{ID: p.Name + "/" + m, Object: "model", OwnedBy: p.Name})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": list})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	req, err := parseChatRequest(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	providerName, modelName, ok := splitModel(req.Model)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model должен иметь вид \"провайдер/модель\", например \"anthropic/claude-opus-5-5\"")
		return
	}
	provider, err := s.store.Provider(providerName)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "invalid_request_error", fmt.Sprintf("провайдер %q не настроен", providerName))
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	adapter, ok := s.adapters[provider.Kind]
	if !ok {
		writeError(w, http.StatusInternalServerError, "server_error", fmt.Sprintf("нет адаптера для типа %q", provider.Kind))
		return
	}
	if msg, err := s.checkBudget(); err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	} else if msg != "" {
		writeError(w, http.StatusTooManyRequests, "budget_exceeded", msg)
		return
	}

	out := newResponder(w, req.Stream, req.Model, s.now())
	call := Call{Provider: provider, Model: modelName, Request: req, Workdir: r.Header.Get(WorkdirHeader)}
	usage, err := s.callWithRotation(r.Context(), adapter, call, out.emit)

	if usage.PromptTokens+usage.CompletionTokens > 0 {
		rec := store.Usage{
			Provider:     provider.Name,
			Model:        modelName,
			Client:       r.Header.Get(ClientHeader),
			InputTokens:  usage.PromptTokens,
			OutputTokens: usage.CompletionTokens,
			CostUSD:      provider.Cost(usage.PromptTokens, usage.CompletionTokens),
		}
		if err := s.store.RecordUsage(rec); err != nil {
			s.log.Error("не удалось записать расход", "err", err)
		}
	}

	if err != nil {
		s.log.Warn("запрос к провайдеру завершился ошибкой", "provider", provider.Name, "model", modelName, "err", err)
		out.fail(err)
		return
	}
	out.finish(usage)
}

// checkBudget возвращает текст ошибки, если дневной бюджет исчерпан.
func (s *Server) checkBudget() (string, error) {
	raw, err := s.store.Setting(SettingDailyBudget)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	limit, err := strconv.ParseFloat(raw, 64)
	if err != nil || limit <= 0 {
		return "", nil
	}
	now := s.now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	spent, err := s.store.SpentSince(midnight)
	if err != nil {
		return "", err
	}
	if spent >= limit {
		return fmt.Sprintf("дневной бюджет исчерпан: потрачено $%.2f из $%.2f", spent, limit), nil
	}
	return "", nil
}

// callWithRotation перебирает ключи провайдера. Ключ, получивший 429, уходит
// на перерыв; ключ, получивший 401/403, отключается навсегда. Переключение
// возможно только до того, как клиенту ушёл первый байт ответа.
func (s *Server) callWithRotation(ctx context.Context, a Adapter, call Call, emit func(Event) error) (Usage, error) {
	p := call.Provider
	keys, err := s.store.Keys(p.ID)
	if err != nil {
		return Usage{}, err
	}
	if len(keys) == 0 {
		// Локальным провайдерам (Ollama и т.п.) ключ не нужен.
		keys = []store.Key{{ID: -1}}
	}

	emitted := false
	trackedEmit := func(ev Event) error {
		emitted = true
		return emit(ev)
	}

	var lastErr error
	for _, k := range keys {
		if s.coolingDown(k.ID) {
			continue
		}
		call.Key = k.Secret
		usage, err := a.Stream(ctx, call, trackedEmit)
		var ue *UpstreamError
		if err == nil || emitted || !errors.As(err, &ue) {
			return usage, err
		}
		lastErr = err
		switch {
		case ue.Status == http.StatusTooManyRequests:
			s.coolDown(k.ID, ue.RetryAfter)
			s.log.Info("ключ упёрся в лимит, переключаюсь на следующий", "provider", p.Name, "key", k.Label)
		case (ue.Status == http.StatusUnauthorized || ue.Status == http.StatusForbidden) && k.ID > 0:
			if err := s.store.DisableKey(k.ID); err != nil {
				s.log.Error("не удалось отключить ключ", "err", err)
			}
			s.log.Warn("ключ отклонён провайдером и отключён", "provider", p.Name, "key", k.Label)
		default:
			return usage, err
		}
	}
	if lastErr == nil {
		return Usage{}, &UpstreamError{Status: http.StatusTooManyRequests, Message: "все ключи провайдера " + p.Name + " временно на перерыве после лимита"}
	}
	return Usage{}, lastErr
}

func (s *Server) coolingDown(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.cooldowns[id]
	return ok && s.now().Before(until)
}

func (s *Server) coolDown(id int64, d time.Duration) {
	if d <= 0 {
		d = defaultCooldown
	}
	s.mu.Lock()
	s.cooldowns[id] = s.now().Add(d)
	s.mu.Unlock()
}

// responder пишет ответ клиенту: SSE-потоком или одним JSON в конце.
type responder struct {
	w       http.ResponseWriter
	stream  bool
	id      string
	model   string
	created int64

	started      bool
	content      strings.Builder
	toolCalls    []ToolCall
	finishReason string
}

func newResponder(w http.ResponseWriter, stream bool, model string, now time.Time) *responder {
	id := make([]byte, 12)
	rand.Read(id)
	return &responder{w: w, stream: stream, id: "chatcmpl-" + hex.EncodeToString(id), model: model, created: now.Unix()}
}

func (o *responder) emit(ev Event) error {
	if ev.FinishReason != "" {
		o.finishReason = ev.FinishReason
	}
	if !o.stream {
		o.content.WriteString(ev.Delta.Content)
		o.mergeToolCalls(ev.Delta.ToolCalls)
		return nil
	}
	if !o.started {
		o.started = true
		h := o.w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		o.w.WriteHeader(http.StatusOK)
	}
	var fr *string
	if ev.FinishReason != "" {
		fr = &ev.FinishReason
	}
	return o.send(Chunk{ID: o.id, Object: "chat.completion.chunk", Created: o.created, Model: o.model,
		Choices: []ChunkChoice{{Delta: ev.Delta, FinishReason: fr}}})
}

func (o *responder) send(c Chunk) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(o.w, "data: %s\n\n", b); err != nil {
		return err
	}
	if f, ok := o.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// mergeToolCalls склеивает потоковые фрагменты tool_calls в полные вызовы.
func (o *responder) mergeToolCalls(parts []ToolCall) {
	for _, p := range parts {
		idx := len(o.toolCalls)
		if p.Index != nil {
			idx = *p.Index
		}
		for len(o.toolCalls) <= idx {
			o.toolCalls = append(o.toolCalls, ToolCall{Type: "function"})
		}
		tc := &o.toolCalls[idx]
		if p.ID != "" {
			tc.ID = p.ID
		}
		if p.Function.Name != "" {
			tc.Function.Name = p.Function.Name
		}
		tc.Function.Arguments += p.Function.Arguments
	}
}

func (o *responder) finish(u Usage) {
	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	if o.stream {
		if !o.started {
			o.emit(Event{Delta: Delta{Role: "assistant"}})
		}
		o.send(Chunk{ID: o.id, Object: "chat.completion.chunk", Created: o.created, Model: o.model, Choices: []ChunkChoice{}, Usage: &u})
		fmt.Fprint(o.w, "data: [DONE]\n\n")
		return
	}
	msg := CompletionMessage{Role: "assistant", ToolCalls: o.toolCalls}
	if text := o.content.String(); text != "" || len(o.toolCalls) == 0 {
		msg.Content = &text
	}
	for i := range msg.ToolCalls {
		msg.ToolCalls[i].Index = nil
	}
	finish := o.finishReason
	if finish == "" {
		finish = "stop"
	}
	writeJSON(o.w, http.StatusOK, Completion{ID: o.id, Object: "chat.completion", Created: o.created, Model: o.model,
		Choices: []CompletionChoice{{Message: msg, FinishReason: finish}}, Usage: u})
}

func (o *responder) fail(err error) {
	status, kind := http.StatusBadGateway, "upstream_error"
	var ue *UpstreamError
	if errors.As(err, &ue) {
		status = ue.Status
		if status == http.StatusTooManyRequests {
			kind = "rate_limit_error"
		}
	}
	if o.stream && o.started {
		// Заголовки уже ушли — сообщаем об ошибке событием в потоке.
		b, _ := json.Marshal(map[string]any{"error": map[string]string{"type": kind, "message": err.Error()}})
		fmt.Fprintf(o.w, "data: %s\n\n", b)
		return
	}
	writeError(o.w, status, kind, err.Error())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, kind, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"type": kind, "message": msg}})
}
