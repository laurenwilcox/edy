package omniroute

import (
	"os"
	"strings"
	"testing"
)

func TestMCPAttachRules(t *testing.T) {
	if !SupportsMCP([]string{"/usr/local/bin/claude", "-p"}) || SupportsMCP([]string{"agy", "-p"}) || SupportsMCP(nil) {
		t.Fatal("MCP из аргументов умеет только Claude Code")
	}
	for raw, ok := range map[string]bool{
		"http://127.0.0.1:11435/mcp?project=/x": true,
		"http://localhost:1/mcp":                true,
		"https://evil.example/mcp":              false,
		"http://10.0.0.5:11435/mcp":             false,
		"http://127.0.0.1:11435/v1":             false,
	} {
		if err := checkMCPURL(raw); (err == nil) != ok {
			t.Errorf("%s: err = %v", raw, err)
		}
	}
	args, cleanup, err := claudeMCP(MCPAttach{URL: "http://127.0.0.1:1/mcp", Token: "edy-secret"})
	if err != nil {
		t.Fatal(err)
	}
	path := args[1]
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("конфиг с токеном должен быть доступен только владельцу: %v %v", info, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"Authorization":"Bearer edy-secret"`) {
		t.Fatalf("конфиг: %s", data)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("конфиг не удалён")
	}
}
