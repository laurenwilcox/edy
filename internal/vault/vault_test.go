package vault

import (
	"bytes"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestSealOpenRoundTrip(t *testing.T) {
	v, err := New(bytes.Repeat([]byte{7}, keySize))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := v.Seal([]byte("sk-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("sk-secret")) {
		t.Fatal("секрет лежит в шифротексте открытым текстом")
	}
	got, err := v.Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "sk-secret" {
		t.Fatalf("got %q", got)
	}
}

func TestOpenRejectsTampering(t *testing.T) {
	v, _ := New(bytes.Repeat([]byte{1}, keySize))
	sealed, _ := v.Seal([]byte("data"))
	sealed[len(sealed)-1] ^= 0xff
	if _, err := v.Open(sealed); err == nil {
		t.Fatal("ожидалась ошибка при подмене данных")
	}
}

func TestFromKeychainIsStable(t *testing.T) {
	keyring.MockInit()
	a, err := FromKeychain()
	if err != nil {
		t.Fatal(err)
	}
	b, err := FromKeychain()
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := a.Seal([]byte("x"))
	if _, err := b.Open(sealed); err != nil {
		t.Fatal("второй запуск должен получить тот же мастер-ключ")
	}
}
