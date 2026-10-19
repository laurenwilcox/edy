package omniroute

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laurenwilcox/name/internal/store"
	"github.com/laurenwilcox/name/internal/vault"
)

const testToken = "edy-test"

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	v, _ := vault.New(bytes.Repeat([]byte{9}, 32))
	st, err := store.Open(filepath.Join(t.TempDir(), "edy.db"), v)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// fakeOpenAI отвечает 429 на ключ "sk-limited" и стримит ответ на остальные.
type fakeOpenAI struct {
	mu       sync.Mutex
	keysSeen []string
	lastBody map[string]any
}

func (f *fakeOpenAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.keysSeen = append(f.keysSeen, key)
	f.lastBody = body
	f.mu.Unlock()

	switch key {
	case "sk-limited":
		w.Header().Set("Retry-After", "30")
		http.Error(w, `{"error":{"message":"rate limited"}}`, http.StatusTooManyRequests)
		return
	case "sk-revoked":
		http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/embeddings") {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"object":"list","model":%q,"data":[{"index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":1000000,"total_tokens":1000000}}`, body["model"])
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, c := range []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"При"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"вет"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1000000,"completion_tokens":500000,"total_tokens":1500000}}`,
	} {
		fmt.Fprintf(w, "data: %s\n\n", c)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func setupOpenAI(t *testing.T, keys ...string) (*Server, *store.Store, *fakeOpenAI) {
	t.Helper()
	fake := &fakeOpenAI{}
	upstream := httptest.NewServer(fake)
	t.Cleanup(upstream.Close)

	st := newTestStore(t)
	id, err := st.AddProvider(store.Provider{Name: "oa", Kind: store.KindOpenAI, BaseURL: upstream.URL, Models: []string{"gpt-x"}, PriceIn: 1, PriceOut: 2})
	if err != nil {
		t.Fatal(err)
	}
	for i, k := range keys {
		st.AddKey(id, fmt.Sprintf("key%d", i), k)
	}
	return NewServer(st, testToken, nil), st, fake
}

func newAuthedRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set(ClientHeader, "agent-backend")
	return req
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return serve(h, newAuthedRequest(method, path, body))
}

func TestRejectsMissingToken(t *testing.T) {
	srv, _, _ := setupOpenAI(t, "sk-ok")
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestModelsList(t *testing.T) {
	srv, _, _ := setupOpenAI(t, "sk-ok")
	rec := do(t, srv, http.MethodGet, "/v1/models", "")
	if !strings.Contains(rec.Body.String(), `"oa/gpt-x"`) {
		t.Fatalf("body = %s", rec.Body)
	}
}

func TestKeyRotationOn429NonStreaming(t *testing.T) {
	srv, st, fake := setupOpenAI(t, "sk-limited", "sk-ok")
	rec := do(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"oa/gpt-x","messages":[{"role":"user","content":"hi"}],"temperature":0.2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body)
	}
	var c Completion
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if got := *c.Choices[0].Message.Content; got != "Привет" {
		t.Fatalf("content = %q", got)
	}
	if c.Model != "oa/gpt-x" || c.Usage.TotalTokens != 1500000 {
		t.Fatalf("completion = %+v", c)
	}
	if strings.Join(fake.keysSeen, ",") != "sk-limited,sk-ok" {
		t.Fatalf("keys = %v", fake.keysSeen)
	}
	// Поля запроса пробрасываются без потерь, model — без префикса провайдера.
	if fake.lastBody["model"] != "gpt-x" || fake.lastBody["temperature"] != 0.2 {
		t.Fatalf("upstream body = %v", fake.lastBody)
	}

	// Ключ на перерыве: второй запрос сразу идёт на рабочий ключ.
	fake.keysSeen = nil
	do(t, srv, http.MethodPost, "/v1/chat/completions", `{"model":"oa/gpt-x","messages":[{"role":"user","content":"hi"}]}`)
	if strings.Join(fake.keysSeen, ",") != "sk-ok" {
		t.Fatalf("keys = %v", fake.keysSeen)
	}

	// 1M входных * $1 + 0.5M выходных * $2 = $2 за запрос.
	spent, _ := st.SpentSince(time.Now().Add(-time.Minute))
	if spent != 4 {
		t.Fatalf("spent = %v", spent)
	}
}

func TestRevokedKeyIsDisabled(t *testing.T) {
	srv, st, _ := setupOpenAI(t, "sk-revoked", "sk-ok")
	rec := do(t, srv, http.MethodPost, "/v1/chat/completions", `{"model":"oa/gpt-x","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	p, _ := st.Provider("oa")
	keys, _ := st.Keys(p.ID)
	if len(keys) != 1 || keys[0].Secret != "sk-ok" {
		t.Fatalf("keys = %+v", keys)
	}
}

func TestAllKeysLimited(t *testing.T) {
	srv, _, _ := setupOpenAI(t, "sk-limited")
	rec := do(t, srv, http.MethodPost, "/v1/chat/completions", `{"model":"oa/gpt-x","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d", rec.Code)
	}
	rec = do(t, srv, http.MethodPost, "/v1/chat/completions", `{"model":"oa/gpt-x","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "на перерыве") {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body)
	}
}

func TestStreamingSSE(t *testing.T) {
	srv, _, _ := setupOpenAI(t, "sk-ok")
	rec := do(t, srv, http.MethodPost, "/v1/chat/completions", `{"model":"oa/gpt-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	var text strings.Builder
	var sawUsage, sawDone bool
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			sawDone = true
			continue
		}
		var c Chunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatal(err)
		}
		if c.Model != "oa/gpt-x" {
			t.Fatalf("model = %q", c.Model)
		}
		for _, ch := range c.Choices {
			text.WriteString(ch.Delta.Content)
		}
		if c.Usage != nil {
			sawUsage = c.Usage.TotalTokens == 1500000
		}
	}
	if text.String() != "Привет" || !sawUsage || !sawDone {
		t.Fatalf("text=%q usage=%v done=%v\n%s", text.String(), sawUsage, sawDone, rec.Body)
	}
}

func TestDailyBudget(t *testing.T) {
	srv, st, _ := setupOpenAI(t, "sk-ok")
	st.SetSetting(SettingDailyBudget, "3")
	body := `{"model":"oa/gpt-x","messages":[{"role":"user","content":"hi"}]}`
	if rec := do(t, srv, http.MethodPost, "/v1/chat/completions", body); rec.Code != http.StatusOK {
		t.Fatalf("первый запрос: %d", rec.Code)
	}
	do(t, srv, http.MethodPost, "/v1/chat/completions", body) // $4 > $3
	rec := do(t, srv, http.MethodPost, "/v1/chat/completions", body)
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "бюджет") {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body)
	}
}

func TestBadModel(t *testing.T) {
	srv, _, _ := setupOpenAI(t, "sk-ok")
	for _, tc := range []struct {
		model string
		code  int
	}{
		{"gpt-x", http.StatusBadRequest},
		{"missing/gpt-x", http.StatusNotFound},
	} {
		rec := do(t, srv, http.MethodPost, "/v1/chat/completions", `{"model":"`+tc.model+`","messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != tc.code {
			t.Errorf("%s: code = %d", tc.model, rec.Code)
		}
	}
}

// --- Anthropic ---

func TestAnthropicTranslationAndStream(t *testing.T) {
	var got map[string]any
	var gotKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		gotKey = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":20,"output_tokens":1}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Читаю файл"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file","input":{}}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"main.go\"}"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":15}}`,
			`{"type":"message_stop"}`,
		}
		for _, e := range events {
			var typ struct{ Type string }
			json.Unmarshal([]byte(e), &typ)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ.Type, e)
		}
	}))
	defer upstream.Close()

	st := newTestStore(t)
	id, _ := st.AddProvider(store.Provider{Name: "anthropic", Kind: store.KindAnthropic, BaseURL: upstream.URL})
	st.AddKey(id, "main", "sk-ant-test")
	srv := NewServer(st, testToken, nil)

	rec := do(t, srv, http.MethodPost, "/v1/chat/completions", `{
		"model": "anthropic/claude-opus-5-5",
		"temperature": 0.5,
		"messages": [
			{"role": "system", "content": "Ты агент-бэкендер."},
			{"role": "user", "content": [
				{"type": "text", "text": "Что в main.go?"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBORw0K"}}
			]},
			{"role": "assistant", "content": null, "tool_calls": [{"id": "toolu_0", "type": "function", "function": {"name": "ls", "arguments": "{}"}}]},
			{"role": "tool", "tool_call_id": "toolu_0", "content": "main.go"}
		],
		"tools": [{"type": "function", "function": {"name": "read_file", "description": "Прочитать файл",
			"parameters": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"], "additionalProperties": false}}}]
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body)
	}

	if gotKey != "sk-ant-test" {
		t.Fatalf("x-api-key = %q", gotKey)
	}
	if got["model"] != "claude-opus-5-5" || got["max_tokens"] != float64(defaultAnthropicMaxTokens) {
		t.Fatalf("model/max_tokens: %v", got)
	}
	if _, ok := got["temperature"]; ok {
		t.Fatal("temperature не должна уходить в Anthropic")
	}
	if sys := got["system"].([]any)[0].(map[string]any)["text"]; sys != "Ты агент-бэкендер." {
		t.Fatalf("system = %v", got["system"])
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %v", msgs)
	}
	user := msgs[0].(map[string]any)["content"].([]any)
	img := user[1].(map[string]any)["source"].(map[string]any)
	if img["type"] != "base64" || img["media_type"] != "image/png" || img["data"] != "iVBORw0K" {
		t.Fatalf("image = %v", img)
	}
	toolUse := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if toolUse["type"] != "tool_use" || toolUse["id"] != "toolu_0" {
		t.Fatalf("tool_use = %v", toolUse)
	}
	toolResult := msgs[2].(map[string]any)
	if toolResult["role"] != "user" || toolResult["content"].([]any)[0].(map[string]any)["tool_use_id"] != "toolu_0" {
		t.Fatalf("tool_result = %v", toolResult)
	}
	schema := got["tools"].([]any)[0].(map[string]any)["input_schema"].(map[string]any)
	if schema["type"] != "object" || schema["additionalProperties"] != false || schema["required"].([]any)[0] != "path" {
		t.Fatalf("input_schema = %v", schema)
	}

	var c Completion
	json.Unmarshal(rec.Body.Bytes(), &c)
	ch := c.Choices[0]
	if *ch.Message.Content != "Читаю файл" || ch.FinishReason != "tool_calls" {
		t.Fatalf("choice = %+v", ch)
	}
	if len(ch.Message.ToolCalls) != 1 || ch.Message.ToolCalls[0].ID != "toolu_1" ||
		ch.Message.ToolCalls[0].Function.Name != "read_file" || ch.Message.ToolCalls[0].Function.Arguments != `{"path":"main.go"}` {
		t.Fatalf("tool_calls = %+v", ch.Message.ToolCalls)
	}
	if c.Usage.PromptTokens != 20 || c.Usage.CompletionTokens != 15 {
		t.Fatalf("usage = %+v", c.Usage)
	}
}

func TestAnthropicRotationOn429(t *testing.T) {
	var keys []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("X-Api-Key"))
		if r.Header.Get("X-Api-Key") == "limited" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"x\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	st := newTestStore(t)
	id, _ := st.AddProvider(store.Provider{Name: "anthropic", Kind: store.KindAnthropic, BaseURL: upstream.URL})
	st.AddKey(id, "a", "limited")
	st.AddKey(id, "b", "good")
	srv := NewServer(st, testToken, nil)

	rec := do(t, srv, http.MethodPost, "/v1/chat/completions", `{"model":"anthropic/claude-haiku-5-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"content":"ok"`) || !strings.Contains(rec.Body.String(), `"finish_reason":"stop"`) {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body)
	}
	if strings.Join(keys, ",") != "limited,good" {
		t.Fatalf("keys = %v", keys)
	}
}

func TestMediaNeedsCaps(t *testing.T) {
	srv, st, _ := setupOpenAI(t, "sk-test")
	img := `{"model":"oa/gpt-x","messages":[{"role":"user","content":[{"type":"text","text":"что тут?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]}`
	rec := do(t, srv, "POST", "/v1/chat/completions", img)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "не видит картинки") {
		t.Fatalf("без тега «Картинки»: %d %s", rec.Code, rec.Body)
	}
	st.SetCaps("oa", []string{CapTools, CapVision})
	if rec := do(t, srv, "POST", "/v1/chat/completions", img); rec.Code != http.StatusOK {
		t.Fatalf("с тегом: %d %s", rec.Code, rec.Body)
	}
	audio := `{"model":"oa/gpt-x","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"UklGRg==","format":"wav"}}]}]}`
	if rec := do(t, srv, "POST", "/v1/chat/completions", audio); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "не принимает звук") {
		t.Fatalf("звук без тега: %d %s", rec.Code, rec.Body)
	}
}

func TestEmbeddingsEndpoint(t *testing.T) {
	srv, st, fake := setupOpenAI(t, "sk-limited", "sk-good")
	rec := do(t, srv, "POST", "/v1/embeddings", `{"model":"oa/gpt-x","input":["оплата заказа"]}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"embedding":[0.1,0.2]`) || !strings.Contains(rec.Body.String(), `"model":"gpt-x"`) {
		t.Fatalf("эмбеддинги: %d %s", rec.Code, rec.Body)
	}
	if fake.keysSeen[len(fake.keysSeen)-1] != "sk-good" {
		t.Fatalf("ротация ключей: %v", fake.keysSeen)
	}
	spent, _ := st.SpentSince(time.Now().Add(-time.Minute))
	if spent < 0.99 || spent > 1.01 {
		t.Fatalf("расход за 1M токенов по $1: %v", spent)
	}
	st.AddProvider(store.Provider{Name: "cli", Kind: store.KindCLI, Command: []string{"x", "{prompt}"}, Models: []string{"m"}})
	if rec := do(t, srv, "POST", "/v1/embeddings", `{"model":"cli/m","input":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("CLI не умеет эмбеддинги: %d", rec.Code)
	}
}

func TestProviderDailyLimit(t *testing.T) {
	srv, st, _ := setupOpenAI(t, "sk-ok")
	st.SetProviderLimit("oa", store.ProviderLimit{ReqDay: 1})
	body := `{"model":"oa/gpt-x","messages":[{"role":"user","content":"hi"}]}`
	if rec := do(t, srv, http.MethodPost, "/v1/chat/completions", body); rec.Code != http.StatusOK {
		t.Fatalf("первый запрос: %d", rec.Code)
	}
	rec := do(t, srv, http.MethodPost, "/v1/chat/completions", body)
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "limit_exceeded") {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body)
	}
}
