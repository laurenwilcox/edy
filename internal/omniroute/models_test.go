package omniroute

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/laurenwilcox/name/internal/store"
)

func TestListOpenAIModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"data":[{"id":"models/gemini-x"},{"id":"a-model"}]}`))
	}))
	defer srv.Close()
	ids, err := ListModels(context.Background(), store.Provider{Kind: store.KindOpenAI, BaseURL: srv.URL + "/v1"}, "k")
	if err != nil || strings.Join(ids, ",") != "a-model,gemini-x" {
		t.Fatalf("ids = %v, err = %v", ids, err)
	}
	if _, err := ListModels(context.Background(), store.Provider{Kind: store.KindOpenAI, BaseURL: srv.URL + "/v1"}, "wrong"); err == nil {
		t.Fatal("ожидалась ошибка авторизации")
	}
}

func TestListAnthropicModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "sk-ant" {
			http.Error(w, `{"type":"error","error":{"type":"authentication_error","message":"bad"}}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"type":"model","id":"claude-sonnet-5-5","display_name":"S","created_at":"2026-01-01T00:00:00Z"},{"type":"model","id":"claude-opus-5-5","display_name":"O","created_at":"2026-01-01T00:00:00Z"}],"has_more":false,"first_id":"a","last_id":"b"}`))
	}))
	defer srv.Close()
	ids, err := ListModels(context.Background(), store.Provider{Kind: store.KindAnthropic, BaseURL: srv.URL}, "sk-ant")
	if err != nil || strings.Join(ids, ",") != "claude-opus-5-5,claude-sonnet-5-5" {
		t.Fatalf("ids = %v, err = %v", ids, err)
	}
	if _, err := ListModels(context.Background(), store.Provider{Kind: store.KindCLI}, ""); err == nil {
		t.Fatal("для CLI ожидалась ошибка")
	}
}
