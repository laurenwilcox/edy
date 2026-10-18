package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/laurenwilcox/name/internal/agent"
	"github.com/laurenwilcox/name/internal/store"
	"github.com/laurenwilcox/name/internal/tasks"
)

func TestMCPCatalogHidesSecrets(t *testing.T) {
	h, st := setup(t)
	if rec := call(h, "POST", "/api/mcp-servers", `{"name":"gh","type":"http","url":"https://api.githubcopilot.com/mcp/","headers":"Authorization: Bearer ghs_topsecret"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("без описания Оркестратор не поймёт, когда выдавать: %d", rec.Code)
	}
	if rec := call(h, "POST", "/api/mcp-servers", `{"name":"gh","description":"issues","type":"http","url":"https://x","headers":"без двоеточия"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("кривой заголовок: %d", rec.Code)
	}
	if rec := call(h, "POST", "/api/mcp-servers", `{"name":"gh","description":"Issues и PR","type":"http","url":"https://api.githubcopilot.com/mcp/","headers":"Authorization: Bearer ghs_topsecret"}`); rec.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/mcp-servers", `{"name":"pw","description":"Браузер","type":"stdio","command":"npx","args":"@playwright/mcp@latest\n","env":"DEBUG=1"}`); rec.Code != http.StatusCreated {
		t.Fatalf("add stdio: %d %s", rec.Code, rec.Body)
	}
	rec := call(h, "GET", "/api/mcp-servers", "")
	if strings.Contains(rec.Body.String(), "ghs_topsecret") {
		t.Fatal("значения заголовков не должны уходить в интерфейс")
	}
	var res struct {
		Servers []mcpServerJSON `json:"servers"`
		Presets []mcpPreset     `json:"presets"`
	}
	json.Unmarshal(rec.Body.Bytes(), &res)
	if len(res.Servers) != 2 || res.Servers[0].Headers[0] != "Authorization" || res.Servers[1].Args[0] != "@playwright/mcp@latest" || len(res.Presets) == 0 {
		t.Fatalf("каталог: %s", rec.Body)
	}
	call(h, "PUT", "/api/mcp-servers/pw/enabled", `{"enabled":false}`)
	if got := h.orchestratorServers(); len(got) != 1 || got[0].Name != "gh" {
		t.Fatalf("Оркестратор видит только включённые: %+v", got)
	}
	if rec := call(h, "DELETE", "/api/mcp-servers/gh", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d", rec.Code)
	}
	if list, _ := st.MCPServers(); len(list) != 1 {
		t.Fatalf("после удаления: %+v", list)
	}
}

// planner — Оркестратор, который выдаёт исполнителю MCP «docs», и агент,
// который сразу заканчивает.
type planner struct{ system string }

func (p *planner) Chat(ctx context.Context, model string, msgs []agent.Msg, tools []agent.ToolDef) (agent.Msg, error) {
	if tools == nil {
		p.system = msgs[0].Content
		return agent.Msg{Role: "assistant", Content: "<plan>\nЗадача: починить Add.\n1. calc.go.\nMCP: docs\n</plan>"}, nil
	}
	var c agent.ToolCall
	c.ID, c.Type = "c1", "function"
	c.Function.Name, c.Function.Arguments = "replace_in_file", `{"path":"calc.go","old":"a - b","new":"a + b"}`
	if len(msgs) > 2 {
		c.Function.Name, c.Function.Arguments = "finish", `{"summary":"ok"}`
	}
	return agent.Msg{Role: "assistant", ToolCalls: []agent.ToolCall{c}}, nil
}

func TestOrchestratorAssignsMCPFromCatalog(t *testing.T) {
	h, st := setup(t)
	st.AddProvider(store.Provider{Name: "agy", Kind: store.KindCLI, Command: []string{"agy", "{prompt}"}, Models: []string{"m"}})
	st.SaveMCPServer(store.MCPServer{Name: "docs", Description: "документация библиотек", Enabled: true, MCPConfig: store.MCPConfig{Type: store.MCPStdio, Command: "npx"}})
	p := &planner{}
	h.LLM = p
	h.Tasks = tasks.NewManager(p)
	dir := gitProject(t)
	call(h, "POST", "/api/projects", fmt.Sprintf(`{"path":%q}`, dir))

	rec := call(h, "POST", "/api/orchestrator", fmt.Sprintf(`{"project":%q,"model":"agy/m","messages":[{"role":"user","content":"почини"}]}`, dir))
	var r struct {
		Plan string   `json:"plan"`
		MCP  []string `json:"mcp"`
	}
	json.Unmarshal(rec.Body.Bytes(), &r)
	if !strings.Contains(p.system, "- docs — документация библиотек") || len(r.MCP) != 1 || r.MCP[0] != "docs" {
		t.Fatalf("Оркестратор: %s\n%s", rec.Body, p.system)
	}

	body, _ := json.Marshal(map[string]any{"project": dir, "request": "почини", "plan": r.Plan, "models": []string{"agy/m"}})
	var v tasks.View
	json.Unmarshal(call(h, "POST", "/api/tasks", string(body)).Body.Bytes(), &v)
	if len(v.MCP) != 1 || v.MCP[0] != "docs" {
		t.Fatalf("задача получила MCP: %+v", v.MCP)
	}
	deadline := time.Now().Add(time.Minute)
	for v.State == tasks.StateRunning && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		json.Unmarshal(call(h, "GET", "/api/tasks/"+v.ID, "").Body.Bytes(), &v)
	}
	found := false
	for _, e := range v.Events {
		found = found || strings.Contains(e.Text, "не умеет MCP: серверы docs не выданы")
	}
	if !found {
		t.Fatalf("исполнитель без MCP должен получить предупреждение: %+v", v.Events)
	}
}
