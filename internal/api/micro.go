package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/laurenwilcox/name/internal/micro"
)

// SettingMicroModel — модель микро-агентов ("провайдер/модель"); пусто — самая дешёвая.
const SettingMicroModel = "micro.model"

// microModel выбирает модель: явная из запроса, затем из настроек, затем самая дешёвая.
func (h *Handler) microModel(explicit string) string {
	if m := strings.TrimSpace(explicit); m != "" {
		return m
	}
	if m, _ := h.Store.Setting(SettingMicroModel); strings.TrimSpace(m) != "" {
		return strings.TrimSpace(m)
	}
	ps, _ := h.Store.Providers()
	return micro.Cheapest(ps)
}

// runMicro запускает микро-агента с таймаутом и пишет его в журнал агентов.
func (h *Handler) runMicro(ctx context.Context, model string, t micro.Task) (micro.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	return micro.Run(ctx, h.LLM, h.Store, h.microModel(model), t)
}

// microFail переводит ошибку микро-агента в HTTP-ответ.
func microFail(w http.ResponseWriter, err error) {
	if errors.Is(err, micro.ErrNoModel) {
		fail(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	fail(w, http.StatusBadGateway, err.Error())
}

func (h *Handler) microRun(w http.ResponseWriter, r *http.Request) {
	if h.LLM == nil {
		fail(w, http.StatusServiceUnavailable, "модели недоступны")
		return
	}
	var req struct {
		Name   string        `json:"name"`
		Task   string        `json:"task"`
		Input  string        `json:"input"`
		Fields []micro.Field `json:"fields"`
		Model  string        `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	if strings.TrimSpace(req.Task) == "" || len(req.Fields) == 0 {
		fail(w, http.StatusBadRequest, "нужны task и fields")
		return
	}
	if req.Name == "" {
		req.Name = "api"
	}
	res, err := h.runMicro(r.Context(), req.Model, micro.Task{Name: req.Name, Prompt: req.Task, Input: req.Input, Fields: req.Fields})
	if err != nil {
		microFail(w, err)
		return
	}
	reply(w, res)
}

func (h *Handler) microSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut {
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, "некорректный JSON")
			return
		}
		if err := h.Store.SetSetting(SettingMicroModel, strings.TrimSpace(req.Model)); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	chosen, _ := h.Store.Setting(SettingMicroModel)
	reply(w, map[string]string{"model": chosen, "effective": h.microModel("")})
}
