package tasks

import (
	"bytes"
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
	"github.com/laurenwilcox/name/internal/vault"
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
	// Первый шаг каждой задачи — правка, дальше — finish.
	if len(msgs) == 2 {
		return tc("replace_in_file", map[string]string{"path": "calc.go", "old": "a - b", "new": "a + b"}), nil
	}
	return tc("finish", map[string]string{"summary": "Исправил Add"}), nil
}

func newMgr(t *testing.T, llm agent.LLM) (*Manager, *store.Store) {
	t.Helper()
	v, _ := vault.New(bytes.Repeat([]byte{4}, 32))
	st, err := store.Open(filepath.Join(t.TempDir(), "edy.db"), v)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := NewManager(llm, st)
	t.Cleanup(m.Wait)
	return m, st
}

func wait(t *testing.T, m *Manager, id string) View {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if v := m.Get(id, 0); v.State != StateRunning && v.State != StateQueued {
			return v
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("задача не завершилась")
	return View{}
}

func TestStartApproveFlow(t *testing.T) {
	dir := project(t)
	m, _ := newMgr(t, &fixer{})
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
	if l := m.List(dir); len(l) != 1 || l[0].Events != nil || l[0].Diff != "" {
		t.Fatal("List должен отдавать задачи без событий и diff")
	}
}

func TestQueueRejectAndCancel(t *testing.T) {
	dir := project(t)
	f := &fixer{gate: make(chan struct{})}
	m, _ := newMgr(t, f)
	v, err := m.Start(dir, "Почини Add", []string{"m/m"})
	if err != nil || v.State != StateRunning {
		t.Fatalf("первая задача: %+v %v", v.State, err)
	}
	q, err := m.Start(dir, "ещё", []string{"m/m"})
	if err != nil || q.State != StateQueued {
		t.Fatalf("вторая задача в том же проекте встаёт в очередь: %s %v", q.State, err)
	}
	if err := m.Cancel(q.ID); err != nil || m.Get(q.ID, 0).State != StateBacklog {
		t.Fatalf("снять с очереди: %v %s", err, m.Get(q.ID, 0).State)
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
}

func TestQueueWaitsForDecisionAndAutoApprove(t *testing.T) {
	dir := project(t)
	m, _ := newMgr(t, &fixer{})
	a, _ := m.Start(dir, "Почини Add", []string{"m/m"})
	b, _ := m.Start(dir, "Почини Add ещё раз", []string{"m/m"})
	if a = wait(t, m, a.ID); a.State != StateDone {
		t.Fatalf("a: %s %s", a.State, a.Error)
	}
	time.Sleep(200 * time.Millisecond)
	if s := m.Get(b.ID, 0).State; s != StateQueued {
		t.Fatalf("следующая ждёт решения по предыдущей: %s", s)
	}
	if _, err := m.Approve(a.ID); err != nil {
		t.Fatal(err)
	}
	if b = wait(t, m, b.ID); b.State != StateDone {
		t.Fatalf("после Approve очередь продолжилась: %s %s", b.State, b.Error)
	}
	m.Reject(b.ID)

	// Автопринятие: очередь идёт без человека.
	var notes []string
	var mu sync.Mutex
	m.AutoApprove = func(string) bool { return true }
	m.Notify = func(v View, msg string) { mu.Lock(); notes = append(notes, msg); mu.Unlock() }
	c, _ := m.Start(dir, "Почини Add", []string{"m/m"})
	wait(t, m, c.ID)
	for i := 0; i < 100 && m.Get(c.ID, 0).State == StateDone; i++ {
		time.Sleep(50 * time.Millisecond) // автопринятие идёт сразу после «готово»
	}
	m.Wait()
	if c = m.Get(c.ID, 0); c.State != StateApproved {
		for _, e := range c.Events {
			t.Log(e.Kind, e.Text)
		}
		t.Fatalf("автопринятие: %s", c.State)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(notes) != 1 || !strings.Contains(notes[0], "принято автоматически") {
		t.Fatalf("уведомления: %v", notes)
	}
}

func TestBoardPoolSubtasksAndRecover(t *testing.T) {
	dir := project(t)
	m, st := newMgr(t, &fixer{})
	p, err := m.Create(Draft{Project: dir, Request: "Сделать вход по паролю\nподробности"})
	if err != nil || p.State != StateBacklog || p.Title != "Сделать вход по паролю" {
		t.Fatalf("пул: %+v %v", p, err)
	}
	sub, err := m.Create(Draft{Project: dir, Parent: p.ID, Title: "Тесты для входа", Request: "покрыть тестами"})
	if err != nil || sub.Parent != p.ID {
		t.Fatalf("подзадача: %+v %v", sub, err)
	}
	if _, err := m.Create(Draft{Project: "/другой", Parent: p.ID, Request: "x"}); err == nil {
		t.Fatal("подзадача из другого проекта")
	}
	m.Comment(p.ID, "Оркестратор", "Начнём с формы")
	ev := m.Get(p.ID, 0).Events
	if len(ev) != 3 || ev[1].Kind != "subtask" || !strings.Contains(ev[2].Text, "Оркестратор: Начнём с формы") {
		t.Fatalf("история: %+v", ev)
	}
	if _, err := m.Edit(p.ID, "Вход", ""); err != nil || m.Get(p.ID, 0).Title != "Вход" {
		t.Fatal("правка в пуле")
	}
	v, err := m.Enqueue(p.ID, Draft{}, Plan{Goal: "Почини Add", Plan: "1. calc.go", Models: []string{"m/m"}, Options: Options{Skills: []string{}}})
	if err != nil {
		t.Fatal(err)
	}
	if v = wait(t, m, v.ID); v.State != StateDone || v.Plan != "1. calc.go" {
		t.Fatalf("из пула в работу: %s", v.State)
	}

	// Перезапуск Edy: задача, оставшаяся «в работе», откатывается.
	r, _ := m.load(v.ID)
	r.State = StateRunning
	m.save(&r)
	m2 := NewManager(&fixer{}, st)
	m2.Recover()
	got := m2.Get(v.ID, 0)
	if got.State != StateFailed || !strings.Contains(got.Error, "откачен") {
		t.Fatalf("после перезапуска: %s %s", got.State, got.Error)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "calc.go")); string(b) != buggy {
		t.Fatal("оборванная задача должна откатиться")
	}
	if err := m2.Delete(sub.ID); err != nil || len(m2.List(dir)) != 1 {
		t.Fatalf("удаление: %v", err)
	}
}

func TestStartRefusesDirtyProject(t *testing.T) {
	dir := project(t)
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x"), 0o644)
	m, _ := newMgr(t, &fixer{})
	if _, err := m.Start(dir, "x", []string{"m/m"}); err == nil {
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
	m, _ := newMgr(t, &fixer{})
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

func TestReviewGatesAutoApprove(t *testing.T) {
	dir := project(t)
	m, _ := newMgr(t, &fixer{})
	verdict := "issues"
	var seenPatch string
	m.Review = func(ctx context.Context, v View, patch string) (Review, error) {
		seenPatch = patch
		return Review{Model: "r/m", Verdict: verdict, Text: "1. calc.go — нет теста на отрицательные"}, nil
	}
	m.AutoApprove = func(string) bool { return true }
	v, _ := m.Start(dir, "Почини Add", []string{"m/m"})
	wait(t, m, v.ID)
	m.Wait()
	v = m.Get(v.ID, 0)
	if v.State != StateDone || v.Review == nil || v.Review.Verdict != "issues" {
		t.Fatalf("с замечаниями автопринятия нет: %s %+v", v.State, v.Review)
	}
	if !strings.Contains(seenPatch, "-func Add(a, b int) int { return a - b }") || !strings.Contains(seenPatch, "+func Add(a, b int) int { return a + b }") {
		t.Fatalf("ревьюер получает полный diff:\n%s", seenPatch)
	}
	found := false
	for _, e := range v.Events {
		found = found || (e.Kind == "review" && strings.Contains(e.Text, "есть замечания"))
	}
	if !found {
		t.Fatal("замечания в истории")
	}
	m.Reject(v.ID)

	verdict = "ok"
	v, _ = m.Start(dir, "Почини Add", []string{"m/m"})
	wait(t, m, v.ID)
	m.Wait()
	if v = m.Get(v.ID, 0); v.State != StateApproved {
		t.Fatalf("ревью без замечаний — автопринятие: %s", v.State)
	}
}
