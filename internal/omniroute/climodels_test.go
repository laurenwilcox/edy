package omniroute

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/laurenwilcox/name/internal/store"
)

func TestParseModelLines(t *testing.T) {
	agy := "Fetching available models...\ngemini-3.8-flash-high\tGemini 3.8 Flash (High)\nclaude-sonnet-4-6\tClaude Sonnet 4.6\n"
	cursor := "You are not authenticated.\n\nDefault model: grok-4.6\n\nAvailable models:\n  * grok-4.6 (default)\n  - grok-4.5\n"
	opencode := "anthropic/claude-sonnet-4-6\nopenai/gpt-5.5\n"
	cases := map[string][]string{
		agy:      {"gemini-3.8-flash-high", "claude-sonnet-4-6"},
		cursor:   {"grok-4.6", "grok-4.5"},
		opencode: {"anthropic/claude-sonnet-4-6", "openai/gpt-5.5"},
	}
	for in, want := range cases {
		if got := parseModelLines(in); !slices.Equal(got, want) {
			t.Errorf("parseModelLines(%q) = %v, want %v", in, got, want)
		}
	}
}

// fakeCodexModels отвечает на «codex debug models» каталогом в формате codex.
const fakeCodexModels = `#!/bin/sh
[ "$1 $2" = "debug models" ] || exit 2
echo '{"models":[{"slug":"gpt-6-sol","visibility":"list"},{"slug":"codex-auto-review","visibility":"hide"},{"slug":"gpt-5.5","visibility":"list"}]}'
`

func TestListCLIModels(t *testing.T) {
	script := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(script, []byte(fakeCodexModels), 0o755); err != nil {
		t.Fatal(err)
	}
	ids, err := ListModels(context.Background(), store.Provider{Kind: store.KindCLI, Command: []string{script, "exec", "-m", "{model}", "{prompt}"}}, "")
	if err != nil || !slices.Equal(ids, []string{"gpt-6-sol", "gpt-5.5"}) {
		t.Fatalf("codex: %v, %v", ids, err)
	}
	ids, err = ListModels(context.Background(), store.Provider{Kind: store.KindCLI, Command: []string{"/opt/bin/claude", "-p", "{prompt}"}}, "")
	if err != nil || !slices.Equal(ids, claudeAliases) {
		t.Fatalf("claude: %v, %v", ids, err)
	}
	if _, err := ListModels(context.Background(), store.Provider{Kind: store.KindCLI, Command: []string{"qwen", "{prompt}"}}, ""); err == nil {
		t.Fatal("qwen: want error")
	}
}
