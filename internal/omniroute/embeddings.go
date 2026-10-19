package omniroute

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/laurenwilcox/name/internal/store"
)

// handleEmbeddings — /v1/embeddings для смыслового поиска: только
// OpenAI-совместимые провайдеры (OpenAI, Ollama, OpenRouter…). Ротация ключей,
// дневной бюджет и учёт расходов — как у чата.
func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "некорректный JSON")
		return
	}
	var full string
	json.Unmarshal(raw["model"], &full)
	providerName, modelName, ok := splitModel(full)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model должен иметь вид \"провайдер/модель\"")
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
	if provider.Kind != store.KindOpenAI {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "эмбеддинги умеют OpenAI-совместимые провайдеры (OpenAI, Ollama, OpenRouter…)")
		return
	}
	if msg, err := s.checkBudget(); err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	} else if msg != "" {
		writeError(w, http.StatusTooManyRequests, "budget_exceeded", msg)
		return
	}
	raw["model"], _ = json.Marshal(modelName)
	payload, _ := json.Marshal(raw)

	keys, err := s.store.Keys(provider.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	if len(keys) == 0 {
		keys = []store.Key{{ID: -1}}
	}
	base := provider.BaseURL
	if base == "" {
		base = defaultOpenAIBaseURL
	}
	var lastStatus int
	var lastBody []byte
	for _, k := range keys {
		ck := fmt.Sprintf("%s/%d", provider.Name, k.ID)
		if s.coolingDown(ck) {
			continue
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(base, "/")+"/embeddings", bytes.NewReader(payload))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "server_error", err.Error())
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if k.Secret != "" {
			req.Header.Set("Authorization", "Bearer "+k.Secret)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
			return
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		lastStatus, lastBody = resp.StatusCode, data
		s.limits.Observe(provider.Name, resp.Header, resp.StatusCode, time.Now())
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			until := s.coolDown(ck, retryAfter(resp.Header))
			s.limits.Fail(provider.Name, resp.StatusCode, "429 на /embeddings", until, time.Now())
			continue
		case (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && k.ID > 0:
			s.store.DisableKey(k.ID)
			continue
		case resp.StatusCode/100 != 2:
			writeError(w, http.StatusBadGateway, "upstream_error", fmt.Sprintf("провайдер ответил %d: %s", resp.StatusCode, strings.TrimSpace(string(data))))
			return
		}
		var u struct {
			Usage struct {
				PromptTokens int64 `json:"prompt_tokens"`
			} `json:"usage"`
		}
		json.Unmarshal(data, &u)
		if u.Usage.PromptTokens > 0 {
			s.store.RecordUsage(store.Usage{Provider: provider.Name, Model: modelName, Client: r.Header.Get(ClientHeader),
				InputTokens: u.Usage.PromptTokens, CostUSD: provider.Cost(u.Usage.PromptTokens, 0)})
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
		return
	}
	writeError(w, http.StatusTooManyRequests, "rate_limit_error", fmt.Sprintf("все ключи недоступны (последний ответ %d: %s)", lastStatus, strings.TrimSpace(string(lastBody))))
}
