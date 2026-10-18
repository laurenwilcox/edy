package tasks

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laurenwilcox/name/internal/agent"
)

// team — несколько исполнителей: каждый по тексту подзадачи пишет свой файл.
// arrived/release проверяют, что подзадачи действительно идут одновременно.
type team struct {
	mu       sync.Mutex
	conflict bool // обе подзадачи правят одну строку
	inflight int
	maxPar   int
	order    []string
}

func (f *team) Chat(ctx context.Context, model string, msgs []agent.Msg, tools []agent.ToolDef) (agent.Msg, error) {
	goal := msgs[1].Content
	step := "1"
	if strings.Contains(goal, "подзадача 2 из") {
		step = "2"
	} else if strings.Contains(goal, "подзадача 3 из") {
		step = "3"
	}
	if len(msgs) == 2 {
		f.mu.Lock()
		f.inflight++
		if f.inflight > f.maxPar {
			f.maxPar = f.inflight
		}
		f.order = append(f.order, step)
		f.mu.Unlock()
		time.Sleep(300 * time.Millisecond) // даём второй подзадаче начаться
		f.mu.Lock()
		f.inflight--
		f.mu.Unlock()
		if f.conflict {
			return tc("replace_in_file", map[string]string{"path": "calc.go", "old": "a - b", "new": "a + b /* " + step + " */"}), nil
		}
		files := map[string]string{"1": "package calc\n\nfunc Sub(a, b int) int { return a - b }\n", "2": "package calc\n\nfunc Mul(a, b int) int { return a * b }\n", "3": "package calc\n\nfunc Sq(a int) int { return Mul(a, a) }\n"}
		names := map[string]string{"1": "sub.go", "2": "mul.go", "3": "sq.go"}
		return tc("write_file", map[string]string{"path": names[step], "content": files[step]}), nil
	}
	return tc("finish", map[string]string{"summary": "готова часть " + step}), nil
}

// cleanProject — проект, где тесты проходят (эпик добавляет новые функции).
func cleanProject(t *testing.T) string {
	dir := project(t)
	os.WriteFile(filepath.Join(dir, "calc.go"), []byte("package calc\n\nfunc Add(a, b int) int { return a - b + b + b - b }\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "calc_test.go"), []byte(strings.Replace(test, "Add(2, 3) != 5", "Add(2, 3) != 5 && false", 1)), 0o644)
	exec.Command("git", "-C", dir, "commit", "-qam", "зелёные тесты").Run()
	return dir
}

func TestEpicRunsSubtasksInParallelAndMerges(t *testing.T) {
	dir := cleanProject(t)
	llm := &team{}
	m, _ := newMgr(t, llm)
	v, err := m.EnqueueEpic("", Draft{Project: dir, Request: "Добавь Sub и Mul"}, Plan{Goal: "Добавь Sub и Mul", Models: []string{"m/m"}}, []SubPlan{
		{Title: "Sub", Goal: "функция Sub в sub.go", Models: []string{"a/m"}},
		{Title: "Mul", Goal: "функция Mul в mul.go", Models: []string{"b/m"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	v = wait(t, m, v.ID)
	m.Wait()
	v = m.Get(v.ID, 0)
	if v.State != StateDone || v.Kind != KindEpic {
		var ev []string
		for _, e := range v.Events {
			ev = append(ev, e.Kind+": "+e.Text)
		}
		t.Fatalf("эпик: %s %s\n%s", v.State, v.Error, strings.Join(ev, "\n"))
	}
	if llm.maxPar != 2 {
		t.Fatalf("подзадачи должны идти одновременно: максимум %d", llm.maxPar)
	}
	if !strings.Contains(v.Diff, "sub.go") || !strings.Contains(v.Diff, "mul.go") || !strings.Contains(v.Summary, "1. Sub — готова часть 1") {
		t.Fatalf("общий результат: %q / %q", v.Diff, v.Summary)
	}
	// Основная папка не менялась до Approve.
	if _, err := os.Stat(filepath.Join(dir, "sub.go")); !os.IsNotExist(err) {
		t.Fatal("до Approve проект не меняется")
	}
	subs := m.children(v.ID, dir)
	if len(subs) != 2 || subs[0].State != StateMerged || subs[1].State != StateMerged {
		t.Fatalf("подзадачи: %+v", subs)
	}
	a, err := m.Approve(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"sub.go", "mul.go"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("после Approve нет %s", f)
		}
	}
	if out, _ := exec.Command("git", "-C", dir, "log", "-1", "--format=%s").Output(); !strings.Contains(string(out), "Добавь Sub и Mul") {
		t.Fatalf("один коммит эпика: %s (%s)", out, a.Note)
	}
	if _, err := os.Stat(filepath.Join(dir, ".edy", "worktrees", v.ID)); !os.IsNotExist(err) {
		t.Fatal("копии проекта должны удаляться после Approve")
	}
	if out, _ := exec.Command("git", "-C", dir, "branch", "--list", "edy/epic-*").Output(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("ветки эпика удаляются: %s", out)
	}
}

func TestEpicConflictAndDependencies(t *testing.T) {
	dir := cleanProject(t)
	llm := &team{conflict: true}
	m, _ := newMgr(t, llm)
	v, _ := m.EnqueueEpic("", Draft{Project: dir, Request: "Почини Add двумя агентами"}, Plan{Goal: "Почини Add", Models: []string{"m/m"}}, []SubPlan{
		{Title: "Первый", Goal: "правка calc.go", Models: []string{"a/m"}},
		{Title: "Второй", Goal: "та же правка", Models: []string{"b/m"}},
	})
	wait(t, m, v.ID)
	m.Wait()
	v = m.Get(v.ID, 0)
	if v.State != StateNeedsHuman || !strings.Contains(v.Error, "не все подзадачи") {
		t.Fatalf("конфликт: %s %s", v.State, v.Error)
	}
	conflicted := false
	for _, s := range m.children(v.ID, dir) {
		conflicted = conflicted || strings.Contains(s.Error, "конфликт при слиянии")
	}
	if !conflicted {
		t.Fatal("подзадача с конфликтом должна это сказать")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "calc.go")); strings.Contains(string(b), "/*") {
		t.Fatal("основная папка не меняется")
	}

	// Зависимости: третья ждёт первую и вторую и начинается последней.
	llm2 := &team{}
	m2, _ := newMgr(t, llm2)
	v, err := m2.EnqueueEpic("", Draft{Project: dir, Request: "Sub, Mul и Sq"}, Plan{Goal: "Sub, Mul и Sq", Models: []string{"m/m"}}, []SubPlan{
		{Title: "Sub", Goal: "sub.go", Models: []string{"a/m"}},
		{Title: "Mul", Goal: "mul.go", Models: []string{"b/m"}},
		{Title: "Sq", Goal: "sq.go через Mul", Models: []string{"c/m"}, After: []int{2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, m2, v.ID)
	m2.Wait()
	if v = m2.Get(v.ID, 0); v.State != StateDone {
		t.Fatalf("эпик с зависимостью: %s %s", v.State, v.Error)
	}
	if llm2.order[len(llm2.order)-1] != "3" {
		t.Fatalf("Sq стартует после Mul: %v", llm2.order)
	}
	if _, err := m2.EnqueueEpic("", Draft{Project: dir}, Plan{Goal: "x", Models: []string{"m/m"}}, []SubPlan{
		{Title: "a", Goal: "a", Models: []string{"a/m"}, After: []int{2}}, {Title: "b", Goal: "b", Models: []string{"a/m"}},
	}); err == nil {
		t.Fatal("подзадача не может ждать более позднюю")
	}
	m2.Reject(v.ID)
}

func TestEpicRetryAndResplit(t *testing.T) {
	dir := cleanProject(t)
	m, _ := newMgr(t, &team{conflict: true})
	plan := Plan{Goal: "Почини Add", Models: []string{"m/m"}}
	v, _ := m.EnqueueEpic("", Draft{Project: dir, Request: "x"}, plan, []SubPlan{{Title: "a", Goal: "a", Models: []string{"a/m"}}, {Title: "b", Goal: "b", Models: []string{"b/m"}}})
	wait(t, m, v.ID)
	m.Wait()
	if s := m.Get(v.ID, 0).State; s != StateNeedsHuman {
		t.Fatalf("ожидался конфликт: %s", s)
	}
	// Новый план делит иначе — старых подзадач не остаётся.
	m.LLM = &team{}
	v, err := m.EnqueueEpic(v.ID, Draft{}, plan, []SubPlan{{Title: "Sub", Goal: "sub.go", Models: []string{"a/m"}}, {Title: "Mul", Goal: "mul.go", Models: []string{"b/m"}}, {Title: "Sq", Goal: "sq.go", Models: []string{"c/m"}, After: []int{2}}})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(m.children(v.ID, dir)); n != 3 {
		t.Fatalf("подзадачи после нового плана: %d", n)
	}
	wait(t, m, v.ID)
	m.Wait()
	if s := m.Get(v.ID, 0).State; s != StateDone {
		t.Fatalf("повтор: %s %s", s, m.Get(v.ID, 0).Error)
	}
	m.Reject(v.ID)
	// План без деления: задача снова обычная.
	if err := m.Unsplit(v.ID); err != nil || m.Get(v.ID, 0).Kind != "" || len(m.children(v.ID, dir)) != 0 {
		t.Fatalf("unsplit: %v", err)
	}
}
