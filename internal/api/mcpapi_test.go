package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laurenwilcox/name/internal/store"
)

func mcpPost(h http.Handler, project, session, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/mcp?project="+url.QueryEscape(project), strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	if session != "" {
		r.Header.Set("Mcp-Session-Id", session)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestMCPEndpointJournalsSessions(t *testing.T) {
	h, st := setup(t)
	h.Home = t.TempDir()
	proj := t.TempDir()
	os.WriteFile(filepath.Join(proj, "a.txt"), []byte("привет"), 0o644)
	m := h.MCP()

	r := httptest.NewRequest(http.MethodPost, "/mcp?project="+url.QueryEscape(proj), strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("без токена: %d", rec.Code)
	}
	if rec := mcpPost(m, proj, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`); rec.Code != http.StatusNotFound {
		t.Fatalf("проект не добавлен в Edy: %d %s", rec.Code, rec.Body)
	}
	call(h, "POST", "/api/projects", `{"path":"`+proj+`"}`)

	rec = mcpPost(m, proj, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"claude-code","version":"2.1"}}}`)
	session := rec.Header().Get("Mcp-Session-Id")
	if rec.Code != http.StatusOK || session == "" || !strings.Contains(rec.Body.String(), `"serverInfo"`) {
		t.Fatalf("initialize: %d %s %q", rec.Code, rec.Body, session)
	}
	if rec := mcpPost(m, proj, session, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("уведомление: %d", rec.Code)
	}
	rec = mcpPost(m, proj, session, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"a.txt"}}}`)
	if !strings.Contains(rec.Body.String(), "привет") {
		t.Fatalf("read_file: %s", rec.Body)
	}
	if rec := mcpPost(m, proj, "mcp-unknown", `{"jsonrpc":"2.0","id":3,"method":"ping"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("чужая сессия: %d", rec.Code)
	}

	a, err := st.Agent(session)
	if err != nil || a.Role != store.RoleMCP || a.Model != "claude-code 2.1" || a.Project != proj || a.State != store.AgentRunning {
		t.Fatalf("агент в журнале: %+v %v", a, err)
	}
	ev, _ := st.AgentEvents(session, 0)
	if len(ev) != 1 || !strings.HasPrefix(ev[0].Text, "read_file a.txt → привет") {
		t.Fatalf("события: %+v", ev)
	}

	del := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	del.Header.Set("Authorization", "Bearer "+token)
	del.Header.Set("Mcp-Session-Id", session)
	m.ServeHTTP(httptest.NewRecorder(), del)
	if a, _ := st.Agent(session); a.State != store.AgentDone {
		t.Fatalf("после отключения: %+v", a)
	}
}

func TestSkillsAPI(t *testing.T) {
	h, _ := setup(t)
	h.Home = t.TempDir()
	proj := t.TempDir()
	call(h, "POST", "/api/projects", `{"path":"`+proj+`"}`)

	if rec := call(h, "POST", "/api/skills", `{"scope":"global","name":"стиль","description":"Общий стиль","body":"Коротко."}`); rec.Code != http.StatusCreated {
		t.Fatalf("общий скилл: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/skills", `{"scope":"project","project":"`+proj+`","name":"тесты","body":"Табличные тесты."}`); rec.Code != http.StatusCreated {
		t.Fatalf("скилл проекта: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/skills", `{"scope":"project","project":"/nope","name":"x","body":"y"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("неизвестный проект: %d", rec.Code)
	}
	var res skillsJSON
	json.Unmarshal(call(h, "GET", "/api/skills?project="+url.QueryEscape(proj), "").Body.Bytes(), &res)
	if len(res.Skills) != 2 || res.ProjectDir != filepath.Join(proj, ".edy", "skills") {
		t.Fatalf("список: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(proj, ".edy", "skills", "тесты", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if rec := call(h, "DELETE", "/api/skills?scope=global&name="+url.QueryEscape("стиль"), ""); rec.Code != http.StatusOK {
		t.Fatalf("удаление: %d %s", rec.Code, rec.Body)
	}
	json.Unmarshal(call(h, "GET", "/api/skills", "").Body.Bytes(), &res)
	if len(res.Skills) != 0 {
		t.Fatalf("общих скиллов не осталось: %+v", res.Skills)
	}
}
