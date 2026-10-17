package store

import (
	"bytes"
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
