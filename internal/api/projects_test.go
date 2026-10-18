package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laurenwilcox/name/internal/agent"
	"github.com/laurenwilcox/name/internal/store"
	"github.com/laurenwilcox/name/internal/tasks"
)

func gitProject(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("нет git")
	}
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "T", "GIT_AUTHOR_EMAIL": "t@x", "GIT_COMMITTER_NAME": "T", "GIT_COMMITTER_EMAIL": "t@x", "GIT_CONFIG_GLOBAL": os.DevNull} {
		t.Setenv(k, v)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module calc\n\ngo 1.21\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "calc.go"), []byte("package calc\n\nfunc Add(a, b int) int { return a - b }\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "calc_test.go"), []byte("package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n"), 0o644)
	for _, a := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, a...)...).CombinedOutput(); err != nil {
			t.Fatal(err, string(out))
		}
	}
	return dir
}

// brain отвечает за Оркестратора (без tools) и за агента (с tools).
type brain struct {
	mu sync.Mutex
	n  int
}

func (b *brain) Chat(ctx context.Context, model string, msgs []agent.Msg, tools []agent.ToolDef) (agent.Msg, error) {
	if tools == nil {
		return agent.Msg{Role: "assistant", Content: "План готов.\n<plan>\nЗадача: починить Add.\n1. calc.go: a - b → a + b.\nПроверка: go test.\n</plan>"}, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n++
	name, args := "finish", `{"summary":"Исправил Add"}`
	if b.n == 1 {
		name, args = "replace_in_file", `{"path":"calc.go","old":"a - b","new":"a + b"}`
	}
	var c agent.ToolCall
	c.ID, c.Type = fmt.Sprintf("c%d", b.n), "function"
	c.Function.Name, c.Function.Arguments = name, args
	return agent.Msg{Role: "assistant", ToolCalls: []agent.ToolCall{c}}, nil
}

func TestProjectOrchestratorTaskFlow(t *testing.T) {
	h, st := setup(t)
	st.AddProvider(store.Provider{Name: "agy", Kind: store.KindCLI, Command: []string{"agy", "{prompt}"}, Models: []string{"m"}})
	b := &brain{}
	h.LLM = b
	h.Tasks = tasks.NewManager(b, st)
	dir := gitProject(t)

	if rec := call(h, "POST", "/api/projects", `{"path":"relative/path"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("относительный путь: %d", rec.Code)
	}
	rec := call(h, "POST", "/api/projects", fmt.Sprintf(`{"path":%q}`, dir))
	var p projectJSON
	json.Unmarshal(rec.Body.Bytes(), &p)
	if rec.Code != http.StatusOK || !p.Ready || p.Branch != "main" {
		t.Fatalf("add project: %d %s", rec.Code, rec.Body)
	}

	rec = call(h, "POST", "/api/orchestrator", fmt.Sprintf(`{"project":%q,"model":"agy/m","messages":[{"role":"user","content":"почини сложение"}]}`, dir))
	var plan struct{ Text, Plan string }
	json.Unmarshal(rec.Body.Bytes(), &plan)
	if rec.Code != http.StatusOK || !strings.Contains(plan.Plan, "a + b") {
		t.Fatalf("orchestrator: %d %s", rec.Code, rec.Body)
	}

	if rec := call(h, "POST", "/api/tasks", fmt.Sprintf(`{"project":%q,"request":"x","plan":"y","models":["nope/m"]}`, dir)); rec.Code != http.StatusBadRequest {
		t.Fatalf("неизвестная модель: %d", rec.Code)
	}
	if rec := call(h, "POST", "/api/tasks", `{"project":"/not/added","request":"x","models":["agy/m"]}`); rec.Code != http.StatusNotFound {
		t.Fatalf("незнакомый проект: %d", rec.Code)
	}
	body, _ := json.Marshal(map[string]any{"project": dir, "request": "почини сложение", "plan": plan.Plan, "models": []string{"agy/m"}})
	rec = call(h, "POST", "/api/tasks", string(body))
	var v tasks.View
	json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Code != http.StatusCreated || !strings.Contains(v.Goal, "Утверждённый план") {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}

	deadline := time.Now().Add(time.Minute)
	for v.State == tasks.StateRunning && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		json.Unmarshal(call(h, "GET", "/api/tasks/"+v.ID, "").Body.Bytes(), &v)
	}
	if v.State != tasks.StateDone {
		t.Fatalf("state = %s %s", v.State, v.Error)
	}

	var cps []agent.Checkpoint
	json.Unmarshal(call(h, "GET", "/api/projects/checkpoints?path="+dir, "").Body.Bytes(), &cps)
	if len(cps) != 3 {
		t.Fatalf("снимки = %+v", cps)
	}

	rec = call(h, "POST", "/api/tasks/"+v.ID+"/approve", "")
	json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Code != http.StatusOK || v.State != tasks.StateApproved || !strings.HasPrefix(v.Branch, "edy/") {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body)
	}

	// Карта проекта (Context Engine).
	if rec := call(h, "GET", "/api/projects/context?path="+dir, ""); !strings.Contains(rec.Body.String(), "func Add(a, b int) int") {
		t.Fatalf("скелет: %s", rec.Body)
	}
	if rec := call(h, "GET", "/api/projects/context?path="+dir+"&q=Add", ""); !strings.Contains(rec.Body.String(), "calc.go · Add") {
		t.Fatalf("поиск: %s", rec.Body)
	}

	// Откат к исходному снимку из панели «Проект».
	base := cps[len(cps)-1].ID
	if rec := call(h, "POST", "/api/projects/restore", fmt.Sprintf(`{"path":%q,"id":%q}`, dir, base)); rec.Code != http.StatusNoContent {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "calc.go")); !strings.Contains(string(b), "a - b") {
		t.Fatal("откат к снимку №0 должен вернуть исходный код")
	}
	if rec := call(h, "POST", "/api/projects/restore", fmt.Sprintf(`{"path":%q,"id":"--hard"}`, dir)); rec.Code != http.StatusBadRequest {
		t.Fatalf("некорректный id: %d", rec.Code)
	}
}
