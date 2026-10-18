package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type result struct {
	Result struct {
		ProtocolVersion string `json:"protocolVersion"`
		Tools           []struct {
			Name string `json:"name"`
		} `json:"tools"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError  bool                    `json:"isError"`
		Prompts  []struct{ Name string } `json:"prompts"`
		Messages []struct {
			Content struct{ Text string } `json:"content"`
		} `json:"messages"`
	} `json:"result"`
	Error *struct {
		Code int `json:"code"`
	} `json:"error"`
}

func rpc(t *testing.T, s *Server, method string, params any) result {
	t.Helper()
	p, _ := json.Marshal(params)
	raw := s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":`+string(p)+`}`))
	var r result
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("%s: %v (%s)", method, err, raw)
	}
	return r
}

func callTool(t *testing.T, s *Server, name string, args map[string]any) (string, bool) {
	t.Helper()
	r := rpc(t, s, "tools/call", map[string]any{"name": name, "arguments": args})
	if r.Error != nil || len(r.Result.Content) == 0 {
		t.Fatalf("%s: %+v", name, r)
	}
	return r.Result.Content[0].Text, r.Result.IsError
}

func project(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module demo\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "calc.go"), []byte("package demo\n\nfunc Add(a, b int) int { return a + b }\n"), 0o644)
	return dir
}

func TestProtocolAndTools(t *testing.T) {
	s := &Server{Root: project(t), Home: t.TempDir()}
	init := rpc(t, s, "initialize", map[string]any{"protocolVersion": "2025-03-26", "clientInfo": map[string]string{"name": "test"}})
	if init.Result.ProtocolVersion != "2025-03-26" {
		t.Fatalf("версия протокола: %+v", init)
	}
	if out := s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); out != nil {
		t.Fatalf("на уведомление не отвечают: %s", out)
	}
	names := map[string]bool{}
	for _, tl := range rpc(t, s, "tools/list", map[string]any{}).Result.Tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"project_skeleton", "search_code", "read_file", "replace_in_file", "run_checks", "list_skills", "use_skill"} {
		if !names[want] {
			t.Fatalf("нет инструмента %s: %v", want, names)
		}
	}
	if names["finish"] {
		t.Fatal("finish нужен только внутреннему агенту")
	}
	if r := rpc(t, s, "nope", map[string]any{}); r.Error == nil || r.Error.Code != codeMethodNotFound {
		t.Fatalf("неизвестный метод: %+v", r)
	}
	if out, isErr := callTool(t, s, "search_code", map[string]any{"query": "add"}); isErr || !strings.Contains(out, "Add") {
		t.Fatalf("search_code: %s", out)
	}
}

func TestEditsAreSnapshottedAndConfined(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("нужен git")
	}
	root := project(t)
	var steps []string
	busy := false
	s := &Server{Root: root, Home: t.TempDir(), Busy: func() bool { return busy }, OnChange: func(st string) { steps = append(steps, st) }}

	if _, isErr := callTool(t, s, "replace_in_file", map[string]any{"path": "calc.go", "old": "a + b", "new": "b + a"}); isErr {
		t.Fatal("правка не прошла")
	}
	data, _ := os.ReadFile(filepath.Join(root, "calc.go"))
	if !bytes.Contains(data, []byte("b + a")) {
		t.Fatalf("файл: %s", data)
	}
	if len(steps) != 1 || steps[0] != "MCP: replace_in_file calc.go" {
		t.Fatalf("снимки: %v", steps)
	}
	log, err := exec.Command("git", "--git-dir="+filepath.Join(root, ".edy", "shadow.git"), "log", "--format=%s").Output()
	if err != nil || !strings.Contains(string(log), "MCP: replace_in_file calc.go") {
		t.Fatalf("снимок в Shadow Git: %s %v", log, err)
	}

	if out, isErr := callTool(t, s, "read_file", map[string]any{"path": "../secret"}); !isErr || !strings.Contains(out, "за пределы") {
		t.Fatalf("выход за проект: %s", out)
	}
	busy = true
	if out, isErr := callTool(t, s, "write_file", map[string]any{"path": "x.go", "content": "package demo"}); !isErr || !strings.Contains(out, "задача Edy") {
		t.Fatalf("во время задачи правки запрещены: %s", out)
	}
	if _, isErr := callTool(t, s, "read_file", map[string]any{"path": "calc.go"}); isErr {
		t.Fatal("чтение во время задачи разрешено")
	}
}

func TestSkillsAsToolsAndPrompts(t *testing.T) {
	root, home := project(t), t.TempDir()
	os.MkdirAll(filepath.Join(root, ".edy", "skills", "review"), 0o755)
	os.WriteFile(filepath.Join(root, ".edy", "skills", "review", "SKILL.md"), []byte("---\nname: review\ndescription: Ревью кода\n---\nПроверь ошибки.\n"), 0o644)
	s := &Server{Root: root, Home: home}

	if out, _ := callTool(t, s, "list_skills", nil); !strings.Contains(out, "review — Ревью кода") {
		t.Fatalf("list_skills: %s", out)
	}
	if out, isErr := callTool(t, s, "use_skill", map[string]any{"name": "review"}); isErr || !strings.Contains(out, "Проверь ошибки.") {
		t.Fatalf("use_skill: %s", out)
	}
	if p := rpc(t, s, "prompts/list", map[string]any{}); len(p.Result.Prompts) != 1 || p.Result.Prompts[0].Name != "review" {
		t.Fatalf("prompts/list: %+v", p)
	}
	if p := rpc(t, s, "prompts/get", map[string]any{"name": "review"}); len(p.Result.Messages) != 1 || p.Result.Messages[0].Content.Text != "Проверь ошибки." {
		t.Fatalf("prompts/get: %+v", p)
	}
}

func TestStdio(t *testing.T) {
	s := &Server{Root: project(t), Home: t.TempDir()}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		"не json\n" +
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	var out bytes.Buffer
	if err := s.ServeStdio(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], `"protocolVersion":"2025-06-18"`) ||
		!strings.Contains(lines[1], `"code":-32700`) || lines[2] != `{"jsonrpc":"2.0","id":2,"result":{}}` {
		t.Fatalf("ответы:\n%s", out.String())
	}
}

func TestTaskSessionSeesOnlyGivenSkills(t *testing.T) {
	root := project(t)
	for _, n := range []string{"review", "secret-notes"} {
		os.MkdirAll(filepath.Join(root, ".edy", "skills", n), 0o755)
		os.WriteFile(filepath.Join(root, ".edy", "skills", n, "SKILL.md"), []byte("---\nname: "+n+"\ndescription: "+n+"\n---\nтекст "+n), 0o644)
	}
	s := &Server{Root: root, Home: t.TempDir(), Skills: []string{"review"}}
	if out, _ := callTool(t, s, "list_skills", nil); !strings.Contains(out, "review") || strings.Contains(out, "secret-notes") {
		t.Fatalf("list_skills: %s", out)
	}
	if _, isErr := callTool(t, s, "use_skill", map[string]any{"name": "secret-notes"}); !isErr {
		t.Fatal("невыданный скилл недоступен")
	}
	s.Skills = []string{}
	if out, _ := callTool(t, s, "list_skills", nil); !strings.Contains(out, "скиллы не выданы") {
		t.Fatalf("без скиллов: %s", out)
	}
}
