package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Проект с ошибкой: Add вычитает вместо сложения, тест это ловит.
const (
	goMod   = "module calc\n\ngo 1.21\n"
	buggy   = "package calc\n\nfunc Add(a, b int) int { return a - b }\n"
	fixed   = "package calc\n\nfunc Add(a, b int) int { return a + b }\n"
	calcTst = "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"Add(2, 3) != 5\")\n\t}\n}\n"
)

func gitEnv(t *testing.T) {
	t.Setenv("GIT_AUTHOR_NAME", "Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
}

func newProject(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("нет git")
	}
	gitEnv(t)
	dir := t.TempDir()
	for name, body := range map[string]string{"go.mod": goMod, "calc.go": buggy, "calc_test.go": calcTst} {
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

// scripted — модель, которая отвечает заранее заданными шагами.
type scripted struct {
	mu    sync.Mutex
	steps map[string][]Msg
	seen  map[string][][]Msg
}

func call(name string, args any) Msg {
	b, _ := json.Marshal(args)
	var tc ToolCall
	tc.ID = fmt.Sprintf("call_%s_%d", name, time.Now().UnixNano())
	tc.Type = "function"
	tc.Function.Name = name
	tc.Function.Arguments = string(b)
	return Msg{Role: "assistant", ToolCalls: []ToolCall{tc}}
}

func (s *scripted) Chat(ctx context.Context, model string, msgs []Msg, tools []ToolDef) (Msg, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string][][]Msg{}
	}
	s.seen[model] = append(s.seen[model], append([]Msg(nil), msgs...))
	q := s.steps[model]
	if len(q) == 0 {
		return Msg{}, fmt.Errorf("у модели %s закончился сценарий", model)
	}
	s.steps[model] = q[1:]
	return q[0], nil
}

func fixSteps() []Msg {
	return []Msg{
		call("list_files", map[string]string{}),
		call("read_file", map[string]string{"path": "calc.go"}),
		call("replace_in_file", map[string]string{"path": "calc.go", "old": "a - b", "new": "a + b"}),
		call("finish", map[string]string{"summary": "Исправил сложение"}),
	}
}

// failSteps: модель создаёт мусорный файл и трижды заявляет, что всё готово, не исправив ошибку.
func failSteps() []Msg {
	return []Msg{
		call("write_file", map[string]string{"path": "junk.txt", "content": "мусор"}),
		call("finish", map[string]string{"summary": "готово"}),
		call("finish", map[string]string{"summary": "точно готово"}),
		call("finish", map[string]string{"summary": "совсем готово"}),
	}
}

func read(t *testing.T, dir, name string) string {
	b, _ := os.ReadFile(filepath.Join(dir, name))
	return string(b)
}

func gitOut(t *testing.T, dir string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestTaskFixesBugAndApprovesIntoBranch(t *testing.T) {
	dir := newProject(t)
	llm := &scripted{steps: map[string][]Msg{"cheap/m": fixSteps()}}
	var events []Event
	task := &Task{Root: dir, Goal: "Почини функцию сложения", Models: []string{"cheap/m"}, LLM: llm, OnEvent: func(e Event) { events = append(events, e) }}
	res, err := task.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusDone || res.Model != "cheap/m" || !strings.Contains(res.Diff, "calc.go") {
		t.Fatalf("res = %+v", res)
	}
	if read(t, dir, "calc.go") != fixed {
		t.Fatalf("calc.go = %q", read(t, dir, "calc.go"))
	}
	// Результат инструмента вернулся модели.
	last := llm.seen["cheap/m"][2]
	if !strings.Contains(last[len(last)-1].Content, "a - b") {
		t.Fatal("модель не получила содержимое файла")
	}

	cps, _ := Shadow{Root: dir}.List(context.Background())
	if len(cps) != 3 || !strings.HasPrefix(cps[0].Message, "Готово") || !strings.HasPrefix(cps[2].Message, "Начало") {
		t.Fatalf("снимки = %+v", cps)
	}

	branch := BranchName(task.Goal, time.Date(2026, 10, 17, 21, 5, 0, 0, time.UTC))
	if branch != "edy/pochini-funkciyu-slozheniya-1017-2105" {
		t.Fatalf("branch = %q", branch)
	}
	if err := Approve(context.Background(), dir, branch, "Edy: "+res.Summary); err != nil {
		t.Fatal(err)
	}
	if got := gitOut(t, dir, "branch", "--show-current"); got != branch {
		t.Fatalf("текущая ветка = %q", got)
	}
	if st := gitOut(t, dir, "status", "--porcelain"); st != "" {
		t.Fatalf("после Approve остались изменения (в т.ч. .edy не должна попасть в git): %q", st)
	}
	if n := gitOut(t, dir, "rev-list", "--count", "main.."+branch); n != "1" {
		t.Fatalf("в ветке %s коммитов, ожидался один", n)
	}
	if files := gitOut(t, dir, "show", "--name-only", "--format=", "HEAD"); files != "calc.go" {
		t.Fatalf("в коммит попало: %q", files)
	}
	if gitOut(t, dir, "show", "main:calc.go") != strings.TrimSpace(buggy) {
		t.Fatal("main не должен меняться")
	}
}

func TestCircuitBreakerEscalatesAndRollsBack(t *testing.T) {
	dir := newProject(t)
	llm := &scripted{steps: map[string][]Msg{"cheap/m": failSteps(), "smart/m": fixSteps()}}
	var log strings.Builder
	task := &Task{Root: dir, Goal: "Почини сложение", Models: []string{"cheap/m", "smart/m"}, LLM: llm,
		OnEvent: func(e Event) { log.WriteString(e.Kind + ": " + e.Text + "\n") }}
	res, err := task.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusDone || res.Model != "smart/m" {
		t.Fatalf("res = %+v\n%s", res, log.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "junk.txt")); !os.IsNotExist(err) {
		t.Fatal("мусор слабой модели должен быть откачен")
	}
	if read(t, dir, "calc.go") != fixed {
		t.Fatal("сильная модель должна была исправить код")
	}
	if !strings.Contains(log.String(), "провалила проверки 3 раза подряд") || !strings.Contains(log.String(), "передана модели smart/m") {
		t.Fatalf("нет событий Circuit Breaker:\n%s", log.String())
	}
	// Сильная модель получила вывод упавших тестов? Нет — она начинает с чистого листа.
	if len(llm.seen["smart/m"][0]) != 2 {
		t.Fatal("сильная модель должна начинать с чистой истории")
	}
	// Слабая модель получила вывод упавшего теста.
	var gotReport bool
	for _, m := range llm.seen["cheap/m"][2] {
		if strings.Contains(m.Content, "Проверки не прошли") && strings.Contains(m.Content, "Add(2, 3) != 5") {
			gotReport = true
		}
	}
	if !gotReport {
		t.Fatal("модель не получила вывод упавших тестов")
	}
}

func TestNeedsHumanRestoresProject(t *testing.T) {
	dir := newProject(t)
	llm := &scripted{steps: map[string][]Msg{"a/m": failSteps(), "b/m": failSteps()}}
	res, err := (&Task{Root: dir, Goal: "x", Models: []string{"a/m", "b/m"}, LLM: llm}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusNeedsHuman {
		t.Fatalf("status = %s", res.Status)
	}
	if read(t, dir, "calc.go") != buggy {
		t.Fatal("проект должен вернуться в исходное состояние")
	}
	if _, err := os.Stat(filepath.Join(dir, "junk.txt")); !os.IsNotExist(err) {
		t.Fatal("мусор должен быть удалён")
	}
	if st := gitOut(t, dir, "status", "--porcelain"); st != "" {
		t.Fatalf("рабочая копия должна быть чистой: %q", st)
	}
}

func TestRejectRestoresBaseline(t *testing.T) {
	dir := newProject(t)
	res, err := (&Task{Root: dir, Goal: "x", Models: []string{"m/m"}, LLM: &scripted{steps: map[string][]Msg{"m/m": fixSteps()}}}).Run(context.Background())
	if err != nil || res.Status != StatusDone {
		t.Fatal(err, res.Status)
	}
	if err := Reject(context.Background(), dir, res.Baseline); err != nil {
		t.Fatal(err)
	}
	if read(t, dir, "calc.go") != buggy {
		t.Fatal("Reject должен вернуть исходный код")
	}
}

func TestDirtyRepoIsRefused(t *testing.T) {
	dir := newProject(t)
	os.WriteFile(filepath.Join(dir, "calc.go"), []byte("// мои правки\n"+buggy), 0o644)
	_, err := (&Task{Root: dir, Goal: "x", Models: []string{"m/m"}, LLM: &scripted{}}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "незакоммиченные") {
		t.Fatalf("err = %v", err)
	}
}

func TestWorkspaceConfinement(t *testing.T) {
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644)
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, ".git"), 0o755)
	os.Symlink(outside, filepath.Join(dir, "link"))
	ws := Workspace{Root: dir}
	for _, p := range []string{"../x", "/etc/passwd", ".git/config", ".edy/shadow.git/HEAD", "link/secret", "a/../../x"} {
		if _, _, err := ws.Call("read_file", fmt.Sprintf(`{"path":%q}`, p)); err == nil {
			t.Errorf("путь %q должен быть запрещён", p)
		}
		if _, _, err := ws.Call("write_file", fmt.Sprintf(`{"path":%q,"content":"x"}`, p)); err == nil {
			t.Errorf("запись в %q должна быть запрещена", p)
		}
	}
	if _, changed, err := ws.Call("write_file", `{"path":"sub/new.go","content":"package sub"}`); err != nil || !changed {
		t.Fatalf("запись внутри проекта: %v", err)
	}
	list, _, _ := ws.Call("list_files", `{}`)
	if !strings.Contains(list, "sub/new.go") || strings.Contains(list, ".git") {
		t.Fatalf("list = %q", list)
	}
	if _, _, err := ws.Call("replace_in_file", `{"path":"sub/new.go","old":"","new":"x"}`); err == nil {
		t.Fatal("пустой old должен отклоняться")
	}
}

func TestDetectHooks(t *testing.T) {
	dir := t.TempDir()
	if h, _ := DetectHooks(dir); len(h) != 0 {
		t.Fatalf("пустой проект: %+v", h)
	}
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644)
	h, _ := DetectHooks(dir)
	if len(h) != 2 || h[0].Name != "go vet" || h[1].Name != "go test" {
		t.Fatalf("go: %+v", h)
	}
	os.MkdirAll(filepath.Join(dir, ".edy"), 0o755)
	os.WriteFile(filepath.Join(dir, HooksFile), []byte(`[{"name":"make","cmd":["make","check"]}]`), 0o644)
	if h, _ := DetectHooks(dir); len(h) != 1 || h[0].Cmd[1] != "check" {
		t.Fatalf("свои хуки: %+v", h)
	}
}

func TestOmniRouteClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string    `json:"model"`
			Tools []ToolDef `json:"tools"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("X-Edy-Client") != "agent" || len(body.Tools) != 6 {
			http.Error(w, `{"error":{"message":"плохой запрос"}}`, http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}}]}}]}`)
	}))
	defer srv.Close()
	c := &OmniRoute{BaseURL: srv.URL + "/v1", Token: "tok", Client: "agent"}
	m, err := c.Chat(context.Background(), "p/m", []Msg{{Role: "user", Content: "hi"}}, Workspace{}.Tools())
	if err != nil || len(m.ToolCalls) != 1 || m.ToolCalls[0].Function.Name != "read_file" {
		t.Fatalf("m = %+v err = %v", m, err)
	}
	c.Token = "bad"
	if _, err := c.Chat(context.Background(), "p/m", nil, Workspace{}.Tools()); err == nil || !strings.Contains(err.Error(), "плохой запрос") {
		t.Fatalf("err = %v", err)
	}
}

func TestPlainFolderWithoutGit(t *testing.T) {
	gitEnv(t)
	dir := t.TempDir()
	for name, body := range map[string]string{"go.mod": goMod, "calc.go": buggy, "calc_test.go": calcTst} {
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	}
	var attempts []string
	task := &Task{Root: dir, Goal: "почини", Models: []string{"m/m"}, LLM: &scripted{steps: map[string][]Msg{"m/m": fixSteps()}},
		OnAttempt: func(m string) { attempts = append(attempts, "start "+m) },
		OnAttemptEnd: func(m string, ok bool, note string) {
			attempts = append(attempts, fmt.Sprintf("end %s %v %s", m, ok, note))
		}}
	res, err := task.Run(context.Background())
	if err != nil || res.Status != StatusDone {
		t.Fatal(err, res.Status)
	}
	if read(t, dir, "calc.go") != fixed {
		t.Fatal("правка в папке без git должна сохраниться")
	}
	if strings.Join(attempts, ";") != "start m/m;end m/m true Исправил сложение" {
		t.Fatalf("attempts = %q", attempts)
	}
	if err := Reject(context.Background(), dir, res.Baseline); err != nil || read(t, dir, "calc.go") != buggy {
		t.Fatalf("Reject в папке без git: %v", err)
	}
}
