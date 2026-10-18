package webchat

import (
	"os"
	"testing"
)

// Аккаунт хранится в папке профиля, попадает в Status и удаляется с профилем.
func TestAccountInStatus(t *testing.T) {
	m := NewManager(t.TempDir())
	if err := os.MkdirAll(m.profileDir("gpt"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := m.Status("gpt").Account; got != "" {
		t.Fatalf("до проверки аккаунт = %q, нужен пустой", got)
	}
	if err := m.saveAccount("gpt", "me@gmail.com"); err != nil {
		t.Fatal(err)
	}
	if got := m.Status("gpt").Account; got != "me@gmail.com" {
		t.Fatalf("аккаунт = %q", got)
	}
	if err := m.saveAccount("gpt", ""); err != nil {
		t.Fatal(err)
	}
	if got := m.Status("gpt").Account; got != "" {
		t.Fatalf("после стирания аккаунт = %q", got)
	}
	m.saveAccount("gpt", "me@gmail.com")
	if err := m.RemoveProfile("gpt"); err != nil {
		t.Fatal(err)
	}
	if got := m.Status("gpt").Account; got != "" {
		t.Fatalf("после удаления профиля аккаунт = %q", got)
	}
}
