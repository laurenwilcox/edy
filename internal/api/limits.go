package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/laurenwilcox/name/internal/agent"
	"github.com/laurenwilcox/name/internal/omniroute"
	"github.com/laurenwilcox/name/internal/store"
)

// LimitSource — лимиты, которые OmniRoute узнал из ответов провайдеров.
type LimitSource interface {
	Limits() map[string]omniroute.RateLimit
}

// checkTimeout — сколько ждать ответа на проверочный запрос.
const checkTimeout = 90 * time.Second

// limitsJSON — лимиты провайдера для экрана настроек.
type limitsJSON struct {
	Upstream  *omniroute.RateLimit `json:"upstream,omitempty"` // из заголовков и ошибок провайдера
	User      store.ProviderLimit  `json:"user"`               // заданные пользователем
	ReqToday  int64                `json:"req_today"`
	TokToday  int64                `json:"tok_today"`
	CheckedMS int64                `json:"checked_ms,omitempty"` // время проверочного запроса
	CheckErr  string               `json:"check_error,omitempty"`
}

func (h *Handler) providerLimits(name string, upstream map[string]omniroute.RateLimit) (limitsJSON, error) {
	var out limitsJSON
	if rl, ok := upstream[name]; ok {
		out.Upstream = &rl
	}
	var err error
	if out.User, err = h.Store.ProviderLimit(name); err != nil {
		return out, err
	}
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	out.ReqToday, out.TokToday, err = h.Store.ProviderUsageSince(name, midnight)
	return out, err
}

func (h *Handler) upstreamLimits() map[string]omniroute.RateLimit {
	if h.Limits == nil {
		return nil
	}
	return h.Limits.Limits()
}

// limits — лимиты всех провайдеров: GET /api/providers/limits.
func (h *Handler) limits(w http.ResponseWriter, r *http.Request) {
	providers, err := h.Store.Providers()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	upstream := h.upstreamLimits()
	out := map[string]limitsJSON{}
	for _, p := range providers {
		if out[p.Name], err = h.providerLimits(p.Name, upstream); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	reply(w, out)
}

// setLimits сохраняет дневные лимиты провайдера: PUT /api/providers/{name}/limits.
func (h *Handler) setLimits(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := h.Store.Provider(name); errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "провайдер не найден")
		return
	}
	var req store.ProviderLimit
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	if err := h.Store.SetProviderLimit(name, req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// checkLimits шлёт провайдеру крошечный запрос через OmniRoute, чтобы получить
// свежие лимиты из заголовков ответа: POST /api/providers/{name}/check.
func (h *Handler) checkLimits(w http.ResponseWriter, r *http.Request) {
	p, err := h.Store.Provider(r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "провайдер не найден")
		return
	} else if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	switch {
	case p.Kind == store.KindWeb:
		fail(w, http.StatusBadRequest, "у веб-чатов лимиты проверяются кнопкой «Проверить вход»")
		return
	case len(p.Models) == 0:
		fail(w, http.StatusBadRequest, "у провайдера не выбрано ни одной модели")
		return
	case h.LLM == nil:
		fail(w, http.StatusServiceUnavailable, "OmniRoute недоступен")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
	defer cancel()
	start := time.Now()
	_, chatErr := h.LLM.Chat(ctx, p.Name+"/"+p.Models[0], []agent.Msg{{Role: "user", Content: "Ответь одним словом: ok"}}, nil)
	out, err := h.providerLimits(p.Name, h.upstreamLimits())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out.CheckedMS = time.Since(start).Milliseconds()
	if chatErr != nil {
		out.CheckErr = chatErr.Error()
	}
	reply(w, out)
}
