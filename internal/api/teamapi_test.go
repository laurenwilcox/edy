package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laurenwilcox/name/internal/agent"
	"github.com/laurenwilcox/name/internal/store"
	"github.com/laurenwilcox/name/internal/tasks"
)

// teamBrain — Оркестратор отдаёт задачу «Бэкенду»; исполнитель проверяет,
// что получил роль агента и его память, и чинит Add.
type teamBrain struct {
	mu     sync.Mutex
	system string
	model  string
}

func (b *teamBrain) Chat(ctx context.Context, model string, msgs []agent.Msg, tools []agent.ToolDef) (agent.Msg, error) {
	if tools == nil {
		b.mu.Lock()
		b.system = msgs[0].Content
		b.mu.Unlock()
		return agent.Msg{Role: "assistant", Content: "<plan>\nЗадача: починить Add.\n1. calc.go\nАгент: бэкенд\n</plan>"}, nil
	}
	b.mu.Lock()
	b.model, b.system = model, msgs[0].Content
	b.mu.Unlock()
	var c agent.ToolCall
	c.ID, c.Type = "c", "function"
	c.Function.Name, c.Function.Arguments = "replace_in_file", `{"path":"calc.go","old":"a - b","new":"a + b"}`
	if len(msgs) > 2 {
		c.Function.Name, c.Function.Arguments = "finish", `{"summary":"Исправил Add"}`
	}
	return agent.Msg{Role: "assistant", ToolCalls: []agent.ToolCall{c}}, nil
}

func TestTeamAgentTakesTask(t *testing.T) {
	h, st := setup(t)
	st.AddProvider(store.Provider{Name: "agy", Kind: store.KindCLI, Command: []string{"agy", "{prompt}"}, Models: []string{"m", "pro"}})
	b := &teamBrain{}
	h.LLM = b
	h.Tasks = tasks.NewManager(b, st)
	h.Tasks.Team, h.Tasks.Notes = st, st
	dir := gitProject(t)
	call(h, "POST", "/api/projects", fmt.Sprintf(`{"path":%q}`, dir))

	rec := call(h, "POST", "/api/team", fmt.Sprintf(`{"project":%q,"name":"Бэкенд","emoji":"🛠","role":"Go-сервер. Пиши табличные тесты.","scope":["*.go"," "],"model":"agy/pro","fallback":"agy/m","enabled":true}`, dir))
	var a store.TeamAgent
	json.Unmarshal(rec.Body.Bytes(), &a)
	if rec.Code != http.StatusCreated || !strings.HasPrefix(a.ID, "agent_") || len(a.Scope) != 1 {
		t.Fatalf("создание: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/team", fmt.Sprintf(`{"project":%q,"name":"бэкенд","model":"agy/m","enabled":true}`, dir)); rec.Code != http.StatusBadRequest {
		t.Fatalf("имя занято: %d", rec.Code)
	}
	if rec := call(h, "POST", "/api/team", fmt.Sprintf(`{"project":%q,"name":"X","model":"nope/m","enabled":true}`, dir)); rec.Code != http.StatusBadRequest {
		t.Fatalf("неизвестная модель: %d", rec.Code)
	}
	call(h, "POST", "/api/memory", fmt.Sprintf(`{"project":%q,"agent":%q,"text":"В calc.go целочисленная арифметика","kind":"fact"}`, dir, a.ID))

	var plan struct{ Plan, Agent string }
	json.Unmarshal(call(h, "POST", "/api/orchestrator", fmt.Sprintf(`{"project":%q,"model":"agy/m","messages":[{"role":"user","content":"почини calc.go"}]}`, dir)).Body.Bytes(), &plan)
	if plan.Agent != "Бэкенд" || !strings.Contains(b.system, "- Бэкенд (agy/pro): Go-сервер; зона: *.go") {
		t.Fatalf("Оркестратор назначил: %q\n%s", plan.Agent, b.system)
	}
	body, _ := json.Marshal(map[string]any{"project": dir, "request": "почини calc.go", "plan": plan.Plan, "models": []string{"agy/m"}})
	var v tasks.View
	json.Unmarshal(call(h, "POST", "/api/tasks", string(body)).Body.Bytes(), &v)
	deadline := time.Now().Add(time.Minute)
	for (v.State == tasks.StateRunning || v.State == tasks.StateQueued) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		json.Unmarshal(call(h, "GET", "/api/tasks/"+v.ID, "").Body.Bytes(), &v)
	}
	b.mu.Lock()
	sys, model := b.system, b.model
	b.mu.Unlock()
	if v.Agent != a.ID || model != "agy/pro" || strings.Join(v.Models, ",") != "agy/pro,agy/m" {
		t.Fatalf("задачу ведёт агент со своей моделью: %q %q %v", v.Agent, model, v.Models)
	}
	for _, want := range []string{"Ты — «Бэкенд», постоянный агент", "Пиши табличные тесты", "Твоя зона: *.go", "Твоя память", "целочисленная арифметика"} {
		if !strings.Contains(sys, want) {
			t.Fatalf("в задании агента нет %q:\n%s", want, sys)
		}
	}
	call(h, "POST", "/api/tasks/"+v.ID+"/approve", "")
	var team []teamAgentJSON
	json.Unmarshal(call(h, "GET", "/api/team?project="+dir, "").Body.Bytes(), &team)
	if len(team) != 1 || team[0].Done != 1 || team[0].Notes != 2 {
		t.Fatalf("статистика и память агента: %+v", team)
	}
	var mem struct{ Notes []store.Note }
	json.Unmarshal(call(h, "GET", "/api/memory?project="+dir+"&agent="+a.ID, "").Body.Bytes(), &mem)
	if len(mem.Notes) != 2 || !strings.Contains(mem.Notes[0].Text, "Исправил Add") {
		t.Fatalf("личная память после задачи: %+v", mem.Notes)
	}
	if rec := call(h, "DELETE", "/api/team/"+a.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("удаление: %d", rec.Code)
	}
	h.Tasks.Wait()
}
