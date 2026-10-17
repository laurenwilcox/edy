package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laurenwilcox/name/internal/agent"
	"github.com/laurenwilcox/name/internal/store"
)

const (
	buggy = "package calc\n\nfunc Add(a, b int) int { return a - b }\n"
	test  = "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n"
)

func project(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("нет git")
	}
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "T", "GIT_AUTHOR_EMAIL": "t@x", "GIT_COMMITTER_NAME": "T", "GIT_COMMITTER_EMAIL": "t@x", "GIT_CONFIG_GLOBAL": os.DevNull} {
		t.Setenv(k, v)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module calc\n\ngo 1.21\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "calc.go"), []byte(buggy), 0o644)
	os.WriteFile(filepath.Join(dir, "calc_test.go"), []byte(test), 0o644)
	for _, a := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, a...)...).CombinedOutput(); err != nil {
			t.Fatal(err, string(out))
		}
	}
	return dir
}

// fixer исправляет ошибку за три шага; если gate задан — ждёт его перед ответом.
type fixer struct {
	mu   sync.Mutex
	n    int
	gate chan struct{}
}

func tc(name string, args map[string]string) agent.Msg {
	b, _ := json.Marshal(args)
	var c agent.ToolCall
	c.ID, c.Type = fmt.Sprintf("c%d", time.Now().UnixNano()), "function"
	c.Function.Name, c.Function.Arguments = name, string(b)
	return agent.Msg{Role: "assistant", ToolCalls: []agent.ToolCall{c}}
}

func (f *fixer) Chat(ctx context.Context, model string, msgs []agent.Msg, tools []agent.ToolDef) (agent.Msg, error) {
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return agent.Msg{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	switch f.n {
	case 1:
		return tc("replace_in_file", map[string]string{"path": "calc.go", "old": "a - b", "new": "a + b"}), nil
	default:
		return tc("finish", map[string]string{"summary": "Исправил Add"}), nil
	}
}

func wait(t *testing.T, m *Manager, id string) View {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if v := m.Get(id, 0); v.State != StateRunning {
			return v
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("задача не завершилась")
	return View{}
}

func TestStartApproveFlow(t *testing.T) {
	dir := project(t)
	m := NewManager(&fixer{})
	v, err := m.Start(dir, "Почини Add", []string{"m/m"})
	if err != nil {
		t.Fatal(err)
	}
	v = wait(t, m, v.ID)
	if v.State != StateDone || v.Summary != "Исправил Add" || !strings.Contains(v.Diff, "calc.go") || len(v.Events) == 0 {
		t.Fatalf("v = %+v", v)
	}
	// Инкрементальная выдача событий.
	if tail := m.Get(v.ID, v.NextFrom); len(tail.Events) != 0 || tail.NextFrom != v.NextFrom {
		t.Fatalf("хвост событий = %+v", tail.Events)
	}
	if !m.Pending(dir) {
		t.Fatal("готовый результат должен ждать решения")
	}
	a, err := m.Approve(v.ID)
	if err != nil || a.State != StateApproved || !strings.HasPrefix(a.Branch, "edy/") {
		t.Fatalf("approve: %+v %v", a, err)
	}
	if _, err := m.Reject(v.ID); err == nil {
		t.Fatal("после Approve Reject невозможен")
	}
	if m.Pending(dir) {
		t.Fatal("после Approve решение больше не ждётся")
	}
	if len(m.List()) != 1 || m.List()[0].Events != nil {
		t.Fatal("List должен отдавать задачи без событий")
	}
}

func TestRejectAndBusyAndCancel(t *testing.T) {
	dir := project(t)
	f := &fixer{gate: make(chan struct{})}
	m := NewManager(f)
	v, err := m.Start(dir, "Почини Add", []string{"m/m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(dir, "ещё", []string{"m/m"}); err != ErrBusy {
		t.Fatalf("вторая задача в том же проекте: %v", err)
	}
	if !m.Running(dir) {
		t.Fatal("проект должен быть занят")
	}
	if err := m.Cancel(v.ID); err != nil {
		t.Fatal(err)
	}
	if v = wait(t, m, v.ID); v.State != StateCancelled {
		t.Fatalf("state = %s", v.State)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "calc.go")); string(b) != buggy {
		t.Fatal("после остановки проект должен вернуться в исходное состояние")
	}

	close(f.gate)
	v, err = m.Start(dir, "Почини Add", []string{"m/m"})
	if err != nil {
		t.Fatal(err)
	}
	if v = wait(t, m, v.ID); v.State != StateDone {
		t.Fatalf("state = %s %s", v.State, v.Error)
	}
	if _, err := m.Reject(v.ID); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "calc.go")); string(b) != buggy {
		t.Fatal("Reject должен откатить правки")
	}
	m.Wait()
}

func TestStartRefusesDirtyProject(t *testing.T) {
	dir := project(t)
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x"), 0o644)
	if _, err := NewManager(&fixer{}).Start(dir, "x", []string{"m/m"}); err == nil {
		t.Fatal("в «грязном» проекте задача не должна стартовать")
	}
}

type memJournal struct {
	mu       sync.Mutex
	agents   []store.Agent
	events   map[string][]string
	finished map[string]string
}

func (j *memJournal) CreateAgent(a store.Agent) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.agents = append(j.agents, a)
	return nil
}
func (j *memJournal) AddAgentEvent(id, kind, text string, at time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.events == nil {
		j.events = map[string][]string{}
	}
	j.events[id] = append(j.events[id], kind+": "+text)
	return nil
}
func (j *memJournal) FinishAgent(id, state, summary, errText string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.finished == nil {
		j.finished = map[string]string{}
	}
	j.finished[id] = state + "|" + summary + errText
	return nil
}

func TestJournalAndCustomFinalize(t *testing.T) {
	dir := project(t)
	j := &memJournal{}
	m := NewManager(&fixer{})
	m.Journal = j
	var finalized string
	m.Finalize = func(ctx context.Context, project, goal, summary, model string) (Outcome, error) {
		finalized = project + "|" + model
		return Outcome{Branch: "main", Note: "коммит в main, отправлено"}, nil
	}
	v, _ := m.Start(dir, "Почини Add", []string{"m/m"})
	v = wait(t, m, v.ID)
	if len(v.Agents) != 1 || len(j.agents) != 1 || j.agents[0].Role != store.RoleExecutor || j.agents[0].Model != "m/m" {
		t.Fatalf("агенты: %+v / %+v", v.Agents, j.agents)
	}
	id := v.Agents[0]
	j.mu.Lock()
	evs := strings.Join(j.events[id], "\n")
	fin := j.finished[id]
	j.mu.Unlock()
	if !strings.Contains(evs, "Снимок №0") || !strings.Contains(evs, "replace_in_file") || !strings.Contains(evs, "✓ go test") {
		t.Fatalf("события агента в журнале:\n%s", evs)
	}
	if fin != "done|Исправил Add" {
		t.Fatalf("итог агента = %q", fin)
	}
	a, err := m.Approve(v.ID)
	if err != nil || finalized != dir+"|m/m" || a.Note != "коммит в main, отправлено" {
		t.Fatalf("approve: %+v %v %q", a, err, finalized)
	}
}
