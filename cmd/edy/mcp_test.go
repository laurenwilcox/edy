package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laurenwilcox/name/internal/api"
	"github.com/laurenwilcox/name/internal/store"
	"github.com/laurenwilcox/name/internal/vault"
)

func TestMCPBridgeToRunningEdy(t *testing.T) {
	v, _ := vault.New(bytes.Repeat([]byte{7}, 32))
	st, err := store.Open(filepath.Join(t.TempDir(), "edy.db"), v)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := api.New(st, "tok", "")
	h.Home = t.TempDir()
	mux := http.NewServeMux()
	mux.Handle("/api/", h)
	mux.Handle("/mcp", h.MCP())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	proj := t.TempDir()
	os.WriteFile(filepath.Join(proj, "a.txt"), []byte("hello"), 0o644)
	b := &bridge{base: srv.URL, token: "tok", http: srv.Client()}
	if err := b.register(context.Background(), proj); err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"gemini-cli"}}}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"a.txt"}}}` + "\n")
	var out bytes.Buffer
	if err := b.serve(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "serverInfo") || !strings.Contains(lines[1], "hello") {
		t.Fatalf("ответы:\n%s", out.String())
	}
	agents, _ := st.Agents(10)
	if len(agents) != 1 || agents[0].Model != "gemini-cli" || agents[0].State != store.AgentDone {
		t.Fatalf("сессия в журнале и закрыта по EOF: %+v", agents)
	}
}

func TestMCPWithoutRunningEdy(t *testing.T) {
	proj := t.TempDir()
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")
	var out bytes.Buffer
	if err := runMCP(t.TempDir(), []string{"-project", proj, "-addr", "127.0.0.1:1"}, in, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "project_skeleton") {
		t.Fatalf("локальный сервер: %s", out.String())
	}
}
