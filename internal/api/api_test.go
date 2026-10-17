package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laurenwilcox/name/internal/store"
	"github.com/laurenwilcox/name/internal/vault"
	"github.com/laurenwilcox/name/internal/webchat"
)

const token = "edy-test"

func setup(t *testing.T) (*Handler, *store.Store) {
	t.Helper()
	v, _ := vault.New(bytes.Repeat([]byte{5}, 32))
	st, err := store.Open(filepath.Join(t.TempDir(), "edy.db"), v)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, token, "127.0.0.1:11435"), st
}

func call(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestRequiresToken(t *testing.T) {
	h, _ := setup(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/providers", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestProviderAndKeyLifecycleNeverLeaksSecrets(t *testing.T) {
	h, _ := setup(t)
	if rec := call(h, "POST", "/api/providers", `{"name":"anthropic","kind":"anthropic","models":["claude-opus-5-5"," "],"price_in":4,"price_out":20}`); rec.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/providers", `{"name":"anthropic","kind":"anthropic"}`); rec.Code != http.StatusConflict {
		t.Fatalf("дубликат: %d", rec.Code)
	}
	if rec := call(h, "POST", "/api/providers/anthropic/keys", `{"label":"main","secret":"sk-ant-SECRET"}`); rec.Code != http.StatusCreated {
		t.Fatalf("key: %d %s", rec.Code, rec.Body)
	}

	rec := call(h, "GET", "/api/providers", "")
	if strings.Contains(rec.Body.String(), "SECRET") {
		t.Fatal("секрет ключа ушёл в API")
	}
	var list []providerJSON
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 || len(list[0].Models) != 1 || len(list[0].Keys) != 1 || list[0].Keys[0].Label != "main" {
		t.Fatalf("list = %+v", list)
	}

	if rec := call(h, "DELETE", "/api/keys/"+jsonInt(list[0].Keys[0].ID), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete key: %d", rec.Code)
	}
	if rec := call(h, "DELETE", "/api/providers/anthropic", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete provider: %d", rec.Code)
	}
	if rec := call(h, "DELETE", "/api/providers/anthropic", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("повторное удаление: %d", rec.Code)
	}
}

func TestPresetInstallFlow(t *testing.T) {
	h, st := setup(t)
	installed := false
	h.FindExecutable = func(name string) (string, error) {
		if installed {
			return "/Users/x/.local/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	var ran string
	h.Install = func(ctx context.Context, cmd string) (string, error) {
		ran, installed = cmd, true
		return "ok", nil
	}

	body := `{"name":"agy","kind":"cli","preset":"antigravity","models":["claude-sonnet-4-6"]}`
	rec := call(h, "POST", "/api/providers", body)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "antigravity.google/cli/install.sh") {
		t.Fatalf("ожидался 409 с командой установки: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/install", `{"preset":"antigravity"}`); rec.Code != http.StatusOK || !strings.Contains(ran, "antigravity") {
		t.Fatalf("install: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/providers", body); rec.Code != http.StatusCreated {
		t.Fatalf("после установки: %d %s", rec.Code, rec.Body)
	}
	p, _ := st.Provider("agy")
	if p.Command[0] != "/Users/x/.local/bin/agy" {
		t.Fatalf("command = %q", p.Command)
	}
	if rec := call(h, "POST", "/api/install", `{"preset":"rm -rf /"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("произвольная команда: %d", rec.Code)
	}
}

func TestBudgetAndUsage(t *testing.T) {
	h, st := setup(t)
	if rec := call(h, "PUT", "/api/budget", `{"usd":5}`); rec.Code != http.StatusNoContent {
		t.Fatalf("budget: %d", rec.Code)
	}
	if rec := call(h, "PUT", "/api/budget", `{"usd":-1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("отрицательный бюджет: %d", rec.Code)
	}
	st.RecordUsage(store.Usage{Client: "backend", Provider: "p", Model: "m", CostUSD: 1.25})
	var u usageJSON
	json.Unmarshal(call(h, "GET", "/api/usage", "").Body.Bytes(), &u)
	if u.BudgetUSD != 5 || u.TodayUSD != 1.25 || len(u.Items) != 1 || u.Items[0].Client != "backend" {
		t.Fatalf("usage = %+v", u)
	}
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestRemoteModelsAndSetModels(t *testing.T) {
	h, st := setup(t)
	call(h, "POST", "/api/providers", `{"name":"or","kind":"openai","base_url":"https://openrouter.ai/api/v1"}`)
	call(h, "POST", "/api/providers/or/keys", `{"secret":"sk-or-1"}`)
	var gotKey string
	h.ListModels = func(ctx context.Context, p store.Provider, key string) ([]string, error) {
		gotKey = key
		return []string{"a/one", "b/two"}, nil
	}
	rec := call(h, "GET", "/api/providers/or/remote-models", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "b/two") || gotKey != "sk-or-1" {
		t.Fatalf("remote: %d %s key=%q", rec.Code, rec.Body, gotKey)
	}
	if rec := call(h, "PUT", "/api/providers/or/models", `{"models":["a/one"," ","bad,name"]}`); rec.Code != http.StatusNoContent {
		t.Fatalf("set: %d", rec.Code)
	}
	p, _ := st.Provider("or")
	if strings.Join(p.Models, "|") != "a/one" {
		t.Fatalf("models = %q", p.Models)
	}
	if rec := call(h, "PUT", "/api/providers/nope/models", `{"models":[]}`); rec.Code != http.StatusNotFound {
		t.Fatalf("missing: %d", rec.Code)
	}
}

func TestPresetsCatalog(t *testing.T) {
	h, _ := setup(t)
	var out struct {
		CLI []cliPresetJSON `json:"cli"`
		API []apiPresetJSON `json:"api"`
	}
	json.Unmarshal(call(h, "GET", "/api/presets", "").Body.Bytes(), &out)
	if len(out.CLI) < 10 || len(out.API) < 15 {
		t.Fatalf("cli=%d api=%d", len(out.CLI), len(out.API))
	}
}

type fakeWeb struct {
	logged  webchat.Conn
	removed string
}

func (f *fakeWeb) Sites() (map[string]webchat.Site, error) { return webchat.DefaultSites, nil }
func (f *fakeWeb) Login(ctx context.Context, c webchat.Conn) error {
	f.logged = c
	return nil
}
func (f *fakeWeb) Check(ctx context.Context, c webchat.Conn) webchat.Status {
	return webchat.Status{State: webchat.StateOK}
}
func (f *fakeWeb) Status(profile string) webchat.Status {
	if profile == f.logged.Profile {
		return webchat.Status{State: webchat.StateLoginOpen}
	}
	return webchat.Status{State: webchat.StateUnknown}
}
func (f *fakeWeb) RemoveProfile(name string) error   { f.removed = name; return nil }
func (f *fakeWeb) BrowserInstalled(kind string) bool { return kind == webchat.BrowserChrome }

func TestWebProviders(t *testing.T) {
	h, st := setup(t)
	if rec := call(h, "POST", "/api/providers", `{"name":"gpt","kind":"web","site":"chatgpt"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("без менеджера браузеров: %d", rec.Code)
	}
	web := &fakeWeb{}
	h.Web = web
	if rec := call(h, "POST", "/api/providers", `{"name":"gpt","kind":"web","site":"nope"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("неизвестный сайт: %d", rec.Code)
	}
	if rec := call(h, "POST", "/api/providers", `{"name":"gpt","kind":"web","site":"chatgpt"}`); rec.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	p, _ := st.Provider("gpt")
	if p.Site != "chatgpt" || strings.Join(p.Models, ",") != "chat" {
		t.Fatalf("p = %+v", p)
	}
	if rec := call(h, "POST", "/api/providers", `{"name":"gpt2","kind":"web","site":"chatgpt","browser":"chrome"}`); rec.Code != http.StatusCreated {
		t.Fatalf("add chrome: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/providers", `{"name":"gpt3","kind":"web","site":"chatgpt","browser":"safari"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("неизвестный браузер: %d", rec.Code)
	}
	if p2, _ := st.Provider("gpt2"); p2.Browser != "chrome" {
		t.Fatalf("browser = %q", p2.Browser)
	}
	if p.Browser != "chromium" {
		t.Fatalf("по умолчанию должен быть chromium, got %q", p.Browser)
	}
	rec := call(h, "POST", "/api/web/login", `{"provider":"gpt"}`)
	if rec.Code != http.StatusOK || web.logged != (webchat.Conn{Profile: "gpt", Site: "chatgpt", Browser: "chromium"}) || !strings.Contains(rec.Body.String(), "login_open") {
		t.Fatalf("login: %d %s %+v", rec.Code, rec.Body, web.logged)
	}
	if rec := call(h, "GET", "/api/web/status", ""); !strings.Contains(rec.Body.String(), `"gpt":{"state":"login_open"`) || !strings.Contains(rec.Body.String(), `"gpt2":{"state":"unknown"`) {
		t.Fatalf("status: %s", rec.Body)
	}
	if rec := call(h, "POST", "/api/web/check", `{"provider":"gpt"}`); !strings.Contains(rec.Body.String(), `"state":"ok"`) {
		t.Fatalf("check: %s", rec.Body)
	}
	if rec := call(h, "GET", "/api/web/browsers", ""); rec.Body.String() != "{\"chrome\":true,\"chromium\":false}\n" {
		t.Fatalf("browsers: %q", rec.Body)
	}
	if rec := call(h, "POST", "/api/web/login", `{"provider":"nope"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("неизвестный провайдер: %d", rec.Code)
	}
	if rec := call(h, "DELETE", "/api/providers/gpt", ""); rec.Code != http.StatusNoContent || web.removed != "gpt" {
		t.Fatalf("delete: %d removed=%q", rec.Code, web.removed)
	}
	var out struct {
		Web []webPresetJSON `json:"web"`
	}
	json.Unmarshal(call(h, "GET", "/api/presets", "").Body.Bytes(), &out)
	if len(out.Web) != len(webchat.DefaultSites) {
		t.Fatalf("web presets = %+v", out.Web)
	}
}
