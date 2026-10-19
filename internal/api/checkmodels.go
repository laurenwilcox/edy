package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/laurenwilcox/name/internal/agent"
	"github.com/laurenwilcox/name/internal/store"
)

// modelCheck — итог проверки одной модели провайдера.
type modelCheck struct {
	Model  string `json:"model"`
	Status string `json:"status"` // ok, error, skipped
	Error  string `json:"error,omitempty"`
}

// checkModelTimeout — сколько ждать ответа одной модели (думающим нужно время).
const checkModelTimeout = 90 * time.Second

// probeModels отправляет каждой модели провайдера короткий запрос. С
// {"prune":true} в списке остаются только ответившие: модели с любой ошибкой
// (нет доступа, лимит, не та модель для чата) убираются. Модели эмбеддингов
// чатом не проверить — они остаются как есть.
func (h *Handler) probeModels(w http.ResponseWriter, r *http.Request) {
	if h.LLM == nil {
		fail(w, http.StatusServiceUnavailable, "модели недоступны")
		return
	}
	var req struct {
		Prune bool `json:"prune"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	p, err := h.Store.Provider(r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "провайдер не найден")
		return
	} else if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	results := make([]modelCheck, len(p.Models))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4) // не больше 4 запросов разом, чтобы не упереться в лимит в минуту
	for i, m := range p.Models {
		results[i] = modelCheck{Model: m}
		if p.Kind == store.KindWeb || strings.Contains(strings.ToLower(m), "embed") {
			results[i].Status = "skipped"
			continue
		}
		wg.Add(1)
		go func(i int, m string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(r.Context(), checkModelTimeout)
			defer cancel()
			_, err := h.LLM.Chat(ctx, p.Name+"/"+m, []agent.Msg{{Role: "user", Content: "Ответь одним словом: ok"}}, nil)
			if err != nil {
				results[i].Status, results[i].Error = "error", shortError(err.Error())
				return
			}
			results[i].Status = "ok"
		}(i, m)
	}
	wg.Wait()
	out := struct {
		Results []modelCheck `json:"results"`
		Kept    []string     `json:"kept"`
		Removed []string     `json:"removed"`
	}{Results: results, Kept: []string{}, Removed: []string{}}
	for _, c := range results {
		if c.Status == "error" {
			out.Removed = append(out.Removed, c.Model)
		} else {
			out.Kept = append(out.Kept, c.Model)
		}
	}
	if req.Prune && len(out.Removed) > 0 {
		if err := h.Store.SetModels(p.Name, out.Kept); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	reply(w, out)
}

// shortError — первая строка ошибки без JSON-простыни провайдера.
func shortError(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\n{["); i > 0 {
		s = strings.TrimRight(strings.TrimSpace(s[:i]), ":")
	}
	if r := []rune(s); len(r) > 160 {
		s = string(r[:160]) + "…"
	}
	return s
}
