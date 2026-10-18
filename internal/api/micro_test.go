package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/laurenwilcox/name/internal/agent"
	"github.com/laurenwilcox/name/internal/store"
)

// cannedLLM отвечает одной и той же репликой и запоминает модель.
type cannedLLM struct {
	reply string
	model string
	last  []agent.Msg
}

func (c *cannedLLM) Chat(_ context.Context, model string, msgs []agent.Msg, _ []agent.ToolDef) (agent.Msg, error) {
	c.model, c.last = model, msgs
	return agent.Msg{Role: "assistant", Content: c.reply}, nil
}

func TestMicroAPI(t *testing.T) {
	h, st := setup(t)
	body := `{"task":"Оцени срочность","input":"сервер упал","fields":[{"name":"level","type":"string","enum":["low","high"],"required":true}]}`
	if rec := call(h, "POST", "/api/micro", body); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("без LLM: %d", rec.Code)
	}
	llm := &cannedLLM{reply: `{"level":"high"}`}
	h.LLM = llm
	if rec := call(h, "POST", "/api/micro", body); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("без провайдеров: %d %s", rec.Code, rec.Body)
	}
	st.AddProvider(store.Provider{Name: "pricey", Models: []string{"big"}, PriceIn: 10, PriceOut: 30})
	st.AddProvider(store.Provider{Name: "cheap", Models: []string{"mini"}, PriceIn: 0.1, PriceOut: 0.4})

	rec := call(h, "POST", "/api/micro", body)
	var res struct {
		Model    string         `json:"model"`
		Data     map[string]any `json:"data"`
		Attempts int            `json:"attempts"`
	}
	json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != http.StatusOK || res.Model != "cheap/mini" || res.Data["level"] != "high" || llm.model != "cheap/mini" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(llm.last[1].Content, "сервер упал") {
		t.Fatalf("входные данные не дошли: %q", llm.last[1].Content)
	}
	agents, _ := st.Agents(10)
	if len(agents) != 1 || agents[0].Role != store.RoleMicro || agents[0].State != store.AgentDone {
		t.Fatalf("журнал: %+v", agents)
	}

	// Модель из настроек важнее самой дешёвой.
	call(h, "PUT", "/api/micro/settings", `{"model":"pricey/big"}`)
	rec = call(h, "GET", "/api/micro/settings", "")
	if !strings.Contains(rec.Body.String(), `"effective":"pricey/big"`) {
		t.Fatalf("настройки: %s", rec.Body)
	}
	call(h, "POST", "/api/micro", body)
	if llm.model != "pricey/big" {
		t.Fatalf("модель %q", llm.model)
	}

	llm.reply = "не знаю"
	if rec := call(h, "POST", "/api/micro", body); rec.Code != http.StatusBadGateway {
		t.Fatalf("плохой ответ: %d", rec.Code)
	}
	if rec := call(h, "POST", "/api/micro", `{"task":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("без полей: %d", rec.Code)
	}
}
