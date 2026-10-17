package agent

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laurenwilcox/name/internal/omniroute"
	"github.com/laurenwilcox/name/internal/store"
	"github.com/laurenwilcox/name/internal/vault"
)

// fakeAgy — CLI-утилита, которая умеет только текст. Она смотрит, сколько
// результатов инструментов уже в диалоге, и отвечает следующим блоком <tool_call>.
const fakeAgy = `#!/bin/sh
prompt="$2"
n=$(printf '%s' "$prompt" | grep -c '### Результат инструмента')
case "$n" in
  0) echo 'Посмотрю код.'; echo '<tool_call>{"name": "read_file", "arguments": {"path": "calc.go"}}</tool_call>' ;;
  1) echo '<tool_call>{"name": "replace_in_file", "arguments": {"path": "calc.go", "old": "a - b", "new": "a + b"}}</tool_call>' ;;
  *) echo '<tool_call>{"name": "finish", "arguments": {"summary": "Исправил Add"}}</tool_call>' ;;
esac
`

func TestAgentOnTextOnlyCLIThroughOmniRoute(t *testing.T) {
	dir := newProject(t)

	bin := filepath.Join(t.TempDir(), "fake-agy")
	os.WriteFile(bin, []byte(fakeAgy), 0o755)
	v, _ := vault.New(bytes.Repeat([]byte{4}, 32))
	st, err := store.Open(filepath.Join(t.TempDir(), "edy.db"), v)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.AddProvider(store.Provider{Name: "agy", Kind: store.KindCLI, Command: []string{bin, "{model}", "{prompt}"}, Models: []string{"claude-sonnet-4-6"}})

	srv := httptest.NewServer(omniroute.NewServer(st, "tok", nil))
	defer srv.Close()

	var log strings.Builder
	task := &Task{
		Root:    dir,
		Goal:    "Почини Add",
		Models:  []string{"agy/claude-sonnet-4-6"},
		LLM:     &OmniRoute{BaseURL: srv.URL + "/v1", Token: "tok", Client: "agent"},
		OnEvent: func(e Event) { log.WriteString(e.Kind + ": " + e.Text + "\n") },
	}
	res, err := task.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusDone || res.Summary != "Исправил Add" {
		t.Fatalf("res = %+v\n%s", res, log.String())
	}
	if read(t, dir, "calc.go") != fixed {
		t.Fatalf("calc.go = %q\n%s", read(t, dir, "calc.go"), log.String())
	}
	if !strings.Contains(log.String(), "✓ go test") {
		t.Fatalf("тесты должны были пройти:\n%s", log.String())
	}
}
