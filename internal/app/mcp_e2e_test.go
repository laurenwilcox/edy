package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/laurenwilcox/name/internal/store"
	"github.com/laurenwilcox/name/internal/vault"
)

// Поддельный Claude Code: тестовый бинарник, запущенный под именем claude.
// Он читает --mcp-config, как настоящий, и правит проект через MCP Edy.
func TestMain(m *testing.M) {
	if os.Getenv("EDY_FAKE_CLAUDE") == "1" {
		if err := fakeClaude(os.Args[1:]); err != nil {
			fmt.Println("ошибка:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeClaude(args []string) error {
	joined := strings.Join(args, " ")
	for _, want := range []string{"--strict-mcp-config", "--allowedTools mcp__edy", "--disallowedTools Bash Edit Write"} {
		if !strings.Contains(joined, want) {
			return fmt.Errorf("нет флага %q в %q", want, joined)
		}
	}
	var cfgPath string
	for i, a := range args {
		if a == "--mcp-config" && i+1 < len(args) {
			cfgPath = args[i+1]
		}
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	var cfg struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	json.Unmarshal(data, &cfg)
	edy := cfg.MCPServers["edy"]
	// Сервер из каталога, который Оркестратор выдал в плане.
	if want := os.Getenv("EDY_FAKE_WANT_MCP"); want != "" {
		if _, ok := cfg.MCPServers[want]; !ok || !strings.Contains(joined, "mcp__"+want) {
			return fmt.Errorf("нет выданного MCP %s: %s | %s", want, data, joined)
		}
	}
	session := ""
	rpc := func(body string) (string, error) {
		req, _ := http.NewRequest(http.MethodPost, edy.URL, strings.NewReader(body))
		for k, v := range edy.Headers {
			req.Header.Set(k, v)
		}
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
			session = s
		}
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 300 {
			return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
		}
		return string(b), nil
	}
	if _, err := rpc(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"claude-code"}}}`); err != nil {
		return err
	}
	content := "package demo\n\nfunc Sub(a, b int) int { return a - b }\n"
	arg, _ := json.Marshal(map[string]any{"name": "write_file", "arguments": map[string]string{"path": "sub.go", "content": content}})
	out, err := rpc(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":` + string(arg) + `}`)
	if err != nil || strings.Contains(out, `"isError":true`) {
		return fmt.Errorf("write_file: %s %v", out, err)
	}
	fmt.Println("Добавил функцию Sub в sub.go.")
	return nil
}

func TestOrchestratorGivesMCPToClaude(t *testing.T) {
	for _, tool := range []string{"git", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("нужен " + tool)
		}
	}
	bin := t.TempDir()
	self, _ := os.Executable()
	if err := os.Symlink(self, filepath.Join(bin, "claude")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("EDY_FAKE_CLAUDE", "1")
	t.Setenv("EDY_FAKE_WANT_MCP", "docs")

	proj := t.TempDir()
	os.WriteFile(filepath.Join(proj, "go.mod"), []byte("module demo\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(proj, "add.go"), []byte("package demo\n\nfunc Add(a, b int) int { return a + b }\n"), 0o644)
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", proj}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}

	home := t.TempDir()
	v, _ := vault.New(bytes.Repeat([]byte{3}, 32))
	st, err := store.Open(filepath.Join(home, "edy.db"), v)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.AddProvider(store.Provider{Name: "cc", Kind: store.KindCLI, Command: []string{"claude", "-p", "--model", "{model}", "{prompt}"}, Models: []string{"sonnet"}})
	st.SaveMCPServer(store.MCPServer{Name: "docs", Description: "документация", Enabled: true,
		MCPConfig: store.MCPConfig{Type: store.MCPHTTP, URL: "https://docs.example/mcp", Headers: map[string]string{"Authorization": "Bearer docs-token"}}})
	srv, err := Start(st, home, "127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	api := func(method, path, body string, out any) {
		t.Helper()
		req, _ := http.NewRequest(method, "http://"+srv.Addr+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+srv.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 300 {
			t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
		}
		if out != nil {
			json.Unmarshal(b, out)
		}
	}
	api("POST", "/api/projects", `{"path":"`+proj+`"}`, nil)
	var task struct {
		ID     string `json:"id"`
		State  string `json:"state"`
		Model  string `json:"model"`
		Events []struct {
			Kind, Text string
		} `json:"events"`
	}
	api("POST", "/api/tasks", `{"project":"`+proj+`","request":"Добавь Sub","plan":"1. sub.go\nMCP: docs","models":["cc/sonnet"]}`, &task)
	deadline := time.Now().Add(90 * time.Second)
	for task.State == "running" && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		api("GET", "/api/tasks/"+task.ID, "", &task)
	}
	var feed strings.Builder
	for _, e := range task.Events {
		fmt.Fprintf(&feed, "[%s] %s\n", e.Kind, e.Text)
	}
	if task.State != "done" || task.Model != "cc/sonnet" {
		t.Fatalf("задача: %s\n%s", task.State, feed.String())
	}
	for _, want := range []string{"получает MCP: Edy, docs (выдал Оркестратор)", "Снимок №1 — write_file sub.go", "write_file sub.go → ", "Добавил функцию Sub", "✓ go test"} {
		if !strings.Contains(feed.String(), want) {
			t.Fatalf("в ленте нет %q:\n%s", want, feed.String())
		}
	}
	if _, err := os.Stat(filepath.Join(proj, "sub.go")); err != nil {
		t.Fatal("sub.go не создан")
	}
	// Конфиг с токеном удалён после запуска.
	if left, _ := filepath.Glob(filepath.Join(os.TempDir(), "edy-mcp-*.json")); len(left) > 0 {
		t.Fatalf("остались конфиги MCP: %v", left)
	}
	// Сессия исполнителя не создаёт отдельного «внешнего агента».
	agents, _ := st.Agents(10)
	for _, a := range agents {
		if a.Role == store.RoleMCP {
			t.Fatalf("лишняя запись MCP-агента: %+v", a)
		}
	}
}
