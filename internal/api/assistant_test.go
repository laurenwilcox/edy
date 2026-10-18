package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/laurenwilcox/name/internal/agent"
	"github.com/laurenwilcox/name/internal/store"
)

// scriptLLM отвечает по сценарию: каждый ответ — либо вызов api_call, либо текст.
type scriptLLM struct {
	mu    sync.Mutex
	steps []agent.Msg
	seen  [][]agent.Msg
}

func (s *scriptLLM) Chat(_ context.Context, _ string, msgs []agent.Msg, _ []agent.ToolDef) (agent.Msg, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, append([]agent.Msg(nil), msgs...))
	if len(s.steps) == 0 {
		return agent.Msg{Role: "assistant", Content: "Готово"}, nil
	}
	m := s.steps[0]
	s.steps = s.steps[1:]
	return m, nil
}

// toolStep — ответ модели с одним вызовом api_call.
func toolStep(id, method, path, body string) agent.Msg {
	var tc agent.ToolCall
	tc.ID, tc.Type = id, "function"
	tc.Function.Name = "api_call"
	args := fmt.Sprintf(`{"method":%q,"path":%q,"why":"тест"`, method, path)
	if body != "" {
		args += `,"body":` + body
	}
	tc.Function.Arguments = args + "}"
	return agent.Msg{Role: "assistant", ToolCalls: []agent.ToolCall{tc}}
}

func decodeReply(t *testing.T, code int, body []byte) asstReply {
	t.Helper()
	var r asstReply
	if code != http.StatusOK || json.Unmarshal(body, &r) != nil {
		t.Fatalf("%d %s", code, body)
	}
	return r
}

func TestAssistantCSSAndConfirm(t *testing.T) {
	h, st := setup(t)
	if rec := call(h, "POST", "/api/assistant", `{"message":"привет"}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("без LLM: %d", rec.Code)
	}
	st.AddProvider(store.Provider{Name: "p", Models: []string{"m"}})
	llm := &scriptLLM{steps: []agent.Msg{
		toolStep("c1", "PUT", "/api/ui/css", `{"css":".chip{color:red}"}`),
		{Role: "assistant", Content: "Сделал кнопки красными"},
	}}
	h.LLM = llm

	rec := call(h, "POST", "/api/assistant", `{"message":"сделай кнопки красными","screen":"Оформление"}`)
	r := decodeReply(t, rec.Code, rec.Body.Bytes())
	if r.Reply != "Сделал кнопки красными" || !r.CSSChanged || len(r.Actions) != 1 || r.Actions[0].Status >= 300 || r.Session == "" {
		t.Fatalf("ответ: %+v", r)
	}
	if css, _ := st.Setting(SettingUICSS); css != ".chip{color:red}" {
		t.Fatalf("CSS не сохранился: %q", css)
	}
	if sys := llm.seen[0][0].Content; !strings.Contains(sys, "Оформление") || !strings.Contains(sys, "/api/ui/css") {
		t.Fatal("в промпте нет текущего раздела или каталога маршрутов")
	}

	// Опасный вызов (DELETE) ждёт подтверждения; отказ — CSS остаётся.
	llm.steps = []agent.Msg{toolStep("c2", "DELETE", "/api/ui/css", ""), {Role: "assistant", Content: "Хорошо, не трогаю"}}
	rec = call(h, "POST", "/api/assistant", fmt.Sprintf(`{"session":%q,"message":"сбрось стили"}`, r.Session))
	r = decodeReply(t, rec.Code, rec.Body.Bytes())
	if r.Pending == nil || r.Pending.Method != "DELETE" || len(r.Actions) != 0 {
		t.Fatalf("ждали подтверждения: %+v", r)
	}
	rec = call(h, "POST", "/api/assistant/confirm", fmt.Sprintf(`{"session":%q,"approve":false}`, r.Session))
	r = decodeReply(t, rec.Code, rec.Body.Bytes())
	if r.Reply != "Хорошо, не трогаю" || len(r.Actions) != 0 {
		t.Fatalf("отказ: %+v", r)
	}
	if css, _ := st.Setting(SettingUICSS); css == "" {
		t.Fatal("CSS сброшен без подтверждения")
	}
	if rec := call(h, "POST", "/api/assistant/confirm", fmt.Sprintf(`{"session":%q,"approve":true}`, r.Session)); rec.Code != http.StatusConflict {
		t.Fatalf("подтверждать нечего: %d", rec.Code)
	}

	// Согласие — вызов выполняется.
	llm.steps = []agent.Msg{toolStep("c3", "DELETE", "/api/ui/css", ""), {Role: "assistant", Content: "Сбросил"}}
	rec = call(h, "POST", "/api/assistant", fmt.Sprintf(`{"session":%q,"message":"всё-таки сбрось"}`, r.Session))
	r = decodeReply(t, rec.Code, rec.Body.Bytes())
	rec = call(h, "POST", "/api/assistant/confirm", fmt.Sprintf(`{"session":%q,"approve":true}`, r.Session))
	r = decodeReply(t, rec.Code, rec.Body.Bytes())
	if r.Reply != "Сбросил" || len(r.Actions) != 1 || !r.CSSChanged {
		t.Fatalf("согласие: %+v", r)
	}
	if css, _ := st.Setting(SettingUICSS); css != "" {
		t.Fatalf("CSS не сброшен: %q", css)
	}

	// Сессия удаляется; подтверждение по ней — 404.
	if rec := call(h, "DELETE", "/api/assistant/"+r.Session, ""); rec.Code >= 300 {
		t.Fatalf("удаление сессии: %d", rec.Code)
	}
	if rec := call(h, "POST", "/api/assistant/confirm", fmt.Sprintf(`{"session":%q,"approve":true}`, r.Session)); rec.Code != http.StatusNotFound {
		t.Fatalf("после удаления: %d", rec.Code)
	}
}

func TestAssistantForbiddenPaths(t *testing.T) {
	h, st := setup(t)
	st.AddProvider(store.Provider{Name: "p", Models: []string{"m"}})
	llm := &scriptLLM{steps: []agent.Msg{
		toolStep("x1", "GET", "/api/info", ""),
		toolStep("x2", "POST", "/api/assistant", `{"message":"рекурсия"}`),
		toolStep("x3", "GET", "/api/../etc", ""),
	}}
	h.LLM = llm
	rec := call(h, "POST", "/api/assistant", `{"message":"сломай всё"}`)
	r := decodeReply(t, rec.Code, rec.Body.Bytes())
	if len(r.Actions) != 0 || r.Reply != "Готово" {
		t.Fatalf("запрещённые пути выполнились: %+v", r)
	}
	// Модель получила отказ на каждый вызов.
	last := llm.seen[len(llm.seen)-1]
	tools := 0
	for _, m := range last {
		if m.Role == "tool" {
			tools++
		}
	}
	if tools != 3 {
		t.Fatalf("ответов инструмента: %d", tools)
	}
}

func TestUICSSLimits(t *testing.T) {
	h, _ := setup(t)
	if rec := call(h, "PUT", "/api/ui/css", `{"css":"a{}</style><script>"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("</style: %d", rec.Code)
	}
	big, _ := json.Marshal(map[string]string{"css": strings.Repeat("a", maxUICSS+1)})
	if rec := call(h, "PUT", "/api/ui/css", string(big)); rec.Code != http.StatusBadRequest {
		t.Fatalf("размер: %d", rec.Code)
	}
}
