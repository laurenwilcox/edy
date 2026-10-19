package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/laurenwilcox/name/internal/vault"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	v, err := vault.New(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(t.TempDir(), "edy.db"), v)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestProvidersAndEncryptedKeys(t *testing.T) {
	s := openTest(t)
	id, err := s.AddProvider(Provider{Name: "openai", Kind: KindOpenAI, Models: []string{"gpt-a", " gpt-b "}, PriceIn: 1, PriceOut: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddKey(id, "main", "sk-one"); err != nil {
		t.Fatal(err)
	}
	k2, _ := s.AddKey(id, "spare", "sk-two")

	var raw []byte
	if err := s.db.QueryRow(`SELECT secret FROM api_keys WHERE id = ?`, k2).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("sk-two")) {
		t.Fatal("ключ хранится в базе открытым текстом")
	}

	p, err := s.Provider("openai")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Models) != 2 || p.Models[1] != "gpt-b" {
		t.Fatalf("models = %q", p.Models)
	}
	keys, _ := s.Keys(p.ID)
	if len(keys) != 2 || keys[0].Secret != "sk-one" {
		t.Fatalf("keys = %+v", keys)
	}
	s.DisableKey(keys[0].ID)
	keys, _ = s.Keys(p.ID)
	if len(keys) != 1 || keys[0].Secret != "sk-two" {
		t.Fatalf("после отключения keys = %+v", keys)
	}
	if _, err := s.Provider("nope"); err != ErrNotFound {
		t.Fatalf("err = %v", err)
	}
}

func TestCommandRoundTripAndReopen(t *testing.T) {
	v, _ := vault.New(bytes.Repeat([]byte{3}, 32))
	path := filepath.Join(t.TempDir(), "edy.db")
	s, err := Open(path, v)
	if err != nil {
		t.Fatal(err)
	}
	s.AddProvider(Provider{Name: "ollama-cli", Kind: KindCLI, Command: []string{"ollama", "run", "{model}", "{prompt}"}})
	s.AddProvider(Provider{Name: "gpt-web", Kind: KindWeb, Site: "chatgpt", Browser: "chromium"})
	s.Close()

	// Повторное открытие не должно падать на уже применённой миграции.
	s, err = Open(path, v)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := s.Provider("ollama-cli")
	if err != nil || len(p.Command) != 4 || p.Command[3] != "{prompt}" {
		t.Fatalf("p = %+v, err = %v", p, err)
	}
	if w, _ := s.Provider("gpt-web"); w.Site != "chatgpt" || w.Browser != "chromium" {
		t.Fatalf("site = %q browser = %q", w.Site, w.Browser)
	}
}

func TestDuplicateAndDeleteProvider(t *testing.T) {
	s := openTest(t)
	id, _ := s.AddProvider(Provider{Name: "agy", Kind: KindCLI})
	s.AddKey(id, "", "secret")
	if _, err := s.AddProvider(Provider{Name: "agy", Kind: KindCLI}); err != ErrExists {
		t.Fatalf("err = %v", err)
	}
	if err := s.DeleteProvider("agy"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProvider("agy"); err != ErrNotFound {
		t.Fatalf("err = %v", err)
	}
	var keys int
	s.db.QueryRow(`SELECT COUNT(*) FROM api_keys`).Scan(&keys)
	if keys != 0 {
		t.Fatalf("ключи удалённого провайдера остались: %d", keys)
	}
}

func TestUsageAndSettings(t *testing.T) {
	s := openTest(t)
	s.RecordUsage(Usage{Provider: "p", Model: "m", InputTokens: 10, OutputTokens: 5, CostUSD: 0.25})
	s.RecordUsage(Usage{Provider: "p", Model: "m", CostUSD: 0.5})
	spent, err := s.SpentSince(time.Now().Add(-time.Hour))
	if err != nil || spent != 0.75 {
		t.Fatalf("spent = %v, %v", spent, err)
	}
	if _, err := s.Setting("x"); err != ErrNotFound {
		t.Fatal(err)
	}
	s.SetSetting("x", "1")
	s.SetSetting("x", "2")
	if v, _ := s.Setting("x"); v != "2" {
		t.Fatalf("v = %q", v)
	}
}

func TestKeyInfosDeleteKeyAndUsageSince(t *testing.T) {
	s := openTest(t)
	id, _ := s.AddProvider(Provider{Name: "p", Kind: KindOpenAI})
	k1, _ := s.AddKey(id, "main", "sk-1")
	s.AddKey(id, "spare", "sk-2")
	s.DisableKey(k1)
	infos, err := s.KeyInfos(id)
	if err != nil || len(infos) != 2 || !infos[0].Disabled || infos[1].Label != "spare" {
		t.Fatalf("infos = %+v, %v", infos, err)
	}
	if err := s.DeleteKey(k1); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteKey(k1); err != ErrNotFound {
		t.Fatalf("err = %v", err)
	}

	s.RecordUsage(Usage{Client: "backend", Provider: "p", Model: "m", InputTokens: 10, OutputTokens: 1, CostUSD: 0.1})
	s.RecordUsage(Usage{Client: "backend", Provider: "p", Model: "m", InputTokens: 5, OutputTokens: 2, CostUSD: 0.2})
	s.RecordUsage(Usage{Client: "ui-bot", Provider: "p", Model: "m", CostUSD: 0.05})
	sum, err := s.UsageSince(time.Now().Add(-time.Hour))
	if err != nil || len(sum) != 2 {
		t.Fatalf("sum = %+v, %v", sum, err)
	}
	if sum[0].Client != "backend" || sum[0].Requests != 2 || sum[0].InputTokens != 15 {
		t.Fatalf("sum[0] = %+v", sum[0])
	}
}

func TestMCPServersAreEncrypted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edy.db")
	v, _ := vault.New(bytes.Repeat([]byte{9}, 32))
	st, err := Open(path, v)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, bad := range []MCPServer{
		{Name: "edy", MCPConfig: MCPConfig{Type: MCPStdio, Command: "x"}},
		{Name: "Bad Name", MCPConfig: MCPConfig{Type: MCPStdio, Command: "x"}},
		{Name: "gh", MCPConfig: MCPConfig{Type: MCPHTTP, URL: "ftp://x"}},
		{Name: "pw", MCPConfig: MCPConfig{Type: MCPStdio}},
	} {
		if st.SaveMCPServer(bad) == nil {
			t.Fatalf("должно отклоняться: %+v", bad)
		}
	}
	gh := MCPServer{Name: "github", Description: "Issues и PR", Enabled: true,
		MCPConfig: MCPConfig{Type: MCPHTTP, URL: "https://api.githubcopilot.com/mcp/", Headers: map[string]string{"Authorization": "Bearer ghs_supersecret"}}}
	if err := st.SaveMCPServer(gh); err != nil {
		t.Fatal(err)
	}
	pw := MCPServer{Name: "playwright", Enabled: true, MCPConfig: MCPConfig{Type: MCPStdio, Command: "npx", Args: []string{"@playwright/mcp@latest"}}}
	if err := st.SaveMCPServer(pw); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMCPServerEnabled("playwright", false); err != nil {
		t.Fatal(err)
	}
	list, err := st.MCPServers()
	if err != nil || len(list) != 2 || list[0].Headers["Authorization"] != "Bearer ghs_supersecret" || list[1].Enabled || list[1].Args[0] != "@playwright/mcp@latest" {
		t.Fatalf("каталог: %+v %v", list, err)
	}
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte("ghs_supersecret")) || bytes.Contains(raw, []byte("githubcopilot")) {
		t.Fatal("настройки MCP-сервера лежат в базе открытым текстом")
	}
	if err := st.DeleteMCPServer("github"); err != nil || st.DeleteMCPServer("github") != ErrNotFound {
		t.Fatal("удаление")
	}
}

func TestModelStats(t *testing.T) {
	v, _ := vault.New(bytes.Repeat([]byte{9}, 32))
	st, err := Open(filepath.Join(t.TempDir(), "edy.db"), v)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i, state := range []string{AgentDone, AgentDone, AgentFailed, AgentRunning} {
		st.CreateAgent(Agent{ID: "a" + string(rune('0'+i)), Role: RoleExecutor, Model: "cheap/m", State: state})
	}
	st.CreateAgent(Agent{ID: "o1", Role: RoleOrchestrator, Model: "cheap/m"})
	st.CreateAgent(Agent{ID: "b1", Role: RoleExecutor, Model: "smart/m", State: AgentDone})
	st.RecordUsage(Usage{Provider: "smart", Model: "m", InputTokens: 1000, OutputTokens: 500, CostUSD: 0.3})
	st.RecordUsage(Usage{Provider: "smart", Model: "m", InputTokens: 10, OutputTokens: 5, CostUSD: 0.1})
	stats, err := st.ModelStats(time.Now().Add(-time.Hour))
	if err != nil || len(stats) != 2 {
		t.Fatalf("stats: %+v %v", stats, err)
	}
	if c := stats[0]; c.Model != "cheap/m" || c.Attempts != 3 || c.Successes != 2 {
		t.Fatalf("cheap: %+v", c)
	}
	if s := stats[1]; s.Model != "smart/m" || s.Successes != 1 || s.Tokens != 1515 || s.CostUSD < 0.39 {
		t.Fatalf("smart: %+v", s)
	}
}

func TestShowWindow(t *testing.T) {
	s := openTest(t)
	s.AddProvider(Provider{Name: "gpt", Kind: KindWeb, Site: "chatgpt", ShowWindow: true})
	if p, _ := s.Provider("gpt"); !p.ShowWindow {
		t.Fatal("show_window не сохранился при создании")
	}
	if err := s.SetShowWindow("gpt", false); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Provider("gpt"); p.ShowWindow {
		t.Fatal("show_window не сбросился")
	}
	if err := s.SetShowWindow("nope", true); err != ErrNotFound {
		t.Fatalf("err = %v", err)
	}
}

func TestProviderLimitAndUsageSince(t *testing.T) {
	s := openTest(t)
	if l, err := s.ProviderLimit("p"); err != nil || l != (ProviderLimit{}) {
		t.Fatalf("пустой лимит = %+v, %v", l, err)
	}
	if err := s.SetProviderLimit("p", ProviderLimit{ReqDay: -1}); err == nil {
		t.Fatal("отрицательный лимит принят")
	}
	s.SetProviderLimit("p", ProviderLimit{ReqDay: 5, TokDay: 100})
	if l, _ := s.ProviderLimit("p"); l.ReqDay != 5 || l.TokDay != 100 {
		t.Fatalf("лимит = %+v", l)
	}
	s.RecordUsage(Usage{Provider: "p", Model: "m", InputTokens: 10, OutputTokens: 5})
	s.RecordUsage(Usage{Provider: "p", Model: "m", InputTokens: 1})
	s.RecordUsage(Usage{Provider: "q", Model: "m", InputTokens: 99})
	req, tok, err := s.ProviderUsageSince("p", time.Now().Add(-time.Hour))
	if err != nil || req != 2 || tok != 16 {
		t.Fatalf("req = %d tok = %d err = %v", req, tok, err)
	}
}
