package app

import (
	"bytes"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laurenwilcox/name/internal/store"
	"github.com/laurenwilcox/name/internal/vault"
)

func TestStartServesUIAndAPI(t *testing.T) {
	home := t.TempDir()
	v, _ := vault.New(bytes.Repeat([]byte{2}, 32))
	st, err := store.Open(filepath.Join(home, "edy.db"), v)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.CreateAgent(store.Agent{ID: "old", Role: store.RoleExecutor, Model: "m"})

	srv, err := Start(st, home, "127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !Running(srv.Addr) {
		t.Fatal("сервер должен отвечать на /healthz")
	}
	if !strings.HasPrefix(srv.URL(), "http://"+srv.Addr+"/#token=edy-") {
		t.Fatalf("url = %s", srv.URL())
	}
	resp, _ := http.Get("http://" + srv.Addr + "/")
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(page), "<title>Edy</title>") {
		t.Fatal("интерфейс не отдаётся")
	}
	req, _ := http.NewRequest(http.MethodGet, "http://"+srv.Addr+"/api/agents", nil)
	req.Header.Set("Authorization", "Bearer "+srv.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("api: %v %v", resp, err)
	}
	resp.Body.Close()
	if a, _ := st.Agent("old"); a.State != store.AgentInterrupted {
		t.Fatalf("агент прошлого запуска должен быть прерван: %+v", a)
	}
	if _, err := Start(st, home, srv.Addr, nil); err == nil {
		t.Fatal("второй запуск на занятом порту должен вернуть ошибку")
	}
	srv.Close()
	if err := <-srv.Done(); err != nil {
		t.Fatal(err)
	}
	if Running(srv.Addr) {
		t.Fatal("после Close сервер не должен отвечать")
	}
}
