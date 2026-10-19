package omniroute

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/laurenwilcox/name/internal/store"
)

func TestJSONOutputCommand(t *testing.T) {
	cases := map[string]bool{
		"codex exec --json {prompt}":                      true,
		"claude -p --output-format stream-json --verbose": true,
		"claude -p --output-format=json":                  true,
		"gemini -o json -p {prompt}":                      true,
		"claude -p --output-format text":                  false,
		"codex exec {prompt}":                             false,
	}
	for cmd, want := range cases {
		if got := jsonOutputCommand(strings.Fields(cmd)); got != want {
			t.Errorf("%q: got %v, want %v", cmd, got, want)
		}
	}
}

func TestCLIJSONFilterCodex(t *testing.T) {
	now := time.Unix(1000, 0)
	var quotas []Quota
	f := &cliJSONFilter{onQuota: func(q Quota) { quotas = append(quotas, q) }, now: func() time.Time { return now }}
	out := f.Write(`{"type":"thread.started","thread_id":"x"}` + "\n" +
		`{"type":"item.completed","item":{"type":"reasoning","text":"думаю"}}` + "\r\n" +
		`{"type":"item.completed","item":{"type":"agent_message","text":"Привет"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"token_count","rate_limits":{"plan_type":"plus",` +
		`"primary":{"used_percent":42.5,"window_minutes":300,"resets_in_seconds":600},` +
		`"secondary":{"used_percent":10,"window_minutes":10080,"resets_at":5000},` +
		`"credits":{"has_credits":true,"unlimited":false,"balance":"12"}}}}` + "\n" +
		`{"type":"item.completed","item":{"type":"agent_message","text":"мир"}}` + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":5}}`)
	out += f.Flush()
	if out != "Привет\n\nмир" {
		t.Fatalf("text: %q", out)
	}
	if f.usage.PromptTokens != 10 || f.usage.CompletionTokens != 5 {
		t.Fatalf("usage: %+v", f.usage)
	}
	if len(quotas) != 1 {
		t.Fatalf("quotas: %+v", quotas)
	}
	q := quotas[0]
	if q.Plan != "plus" || q.Credits != "12" || len(q.Windows) != 2 {
		t.Fatalf("quota: %+v", q)
	}
	if w := q.Windows[0]; w.Name != "primary" || *w.UsedPercent != 42.5 || w.WindowMinutes != 300 || !w.ResetsAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("primary: %+v", w)
	}
	if w := q.Windows[1]; !w.ResetsAt.Equal(time.Unix(5000, 0)) {
		t.Fatalf("secondary: %+v", w)
	}
	if f.limited {
		t.Fatal("not limited")
	}
}

func TestCLIJSONFilterCodexLimited(t *testing.T) {
	f := &cliJSONFilter{now: time.Now}
	f.Write(`{"type":"token_count","rate_limits":{"primary":{"used_percent":100,"window_minutes":300,"resets_at":7000}}}` + "\n" +
		`{"type":"turn.failed","error":{"message":"You've hit your usage limit"}}` + "\n")
	if !f.limited || !f.resetAt.Equal(time.Unix(7000, 0)) || f.errMsg != "You've hit your usage limit" {
		t.Fatalf("filter: limited=%v reset=%v err=%q", f.limited, f.resetAt, f.errMsg)
	}
}

func TestCLIJSONFilterClaude(t *testing.T) {
	var quotas []Quota
	f := &cliJSONFilter{onQuota: func(q Quota) { quotas = append(quotas, q) }, now: time.Now}
	out := f.Write(`{"type":"system","subtype":"init"}` + "\n" +
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":9000,"rateLimitType":"five_hour","utilization":0.8}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"…"},{"type":"text","text":"Ответ"}]}}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"result":"Ответ","usage":{"input_tokens":3,"cache_read_input_tokens":100,"output_tokens":7}}` + "\n")
	out += f.Flush()
	if out != "Ответ" {
		t.Fatalf("text: %q", out)
	}
	if f.usage.PromptTokens != 103 || f.usage.CompletionTokens != 7 {
		t.Fatalf("usage: %+v", f.usage)
	}
	if len(quotas) != 1 {
		t.Fatalf("quotas: %+v", quotas)
	}
	w := quotas[0].Windows[0]
	if w.Name != "five_hour" || w.Status != "allowed_warning" || w.WindowMinutes != 300 || *w.UsedPercent != 80 || !w.ResetsAt.Equal(time.Unix(9000, 0)) {
		t.Fatalf("window: %+v", w)
	}
}

func TestCLIJSONFilterClaudeRejected(t *testing.T) {
	f := &cliJSONFilter{now: time.Now}
	f.Write(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":9000,"rateLimitType":"seven_day"}}` + "\n" +
		`{"type":"result","is_error":true,"result":"Claude AI usage limit reached"}` + "\n")
	if !f.limited || f.texts != 0 || f.errMsg != "Claude AI usage limit reached" || !f.resetAt.Equal(time.Unix(9000, 0)) {
		t.Fatalf("filter: %+v", f)
	}
}

func TestCLIJSONFilterPassesPlainLines(t *testing.T) {
	f := &cliJSONFilter{now: time.Now}
	if out := f.Write("warning: старый конфиг\n{\"response\":\"ok\"}\n"); out != "warning: старый конфиг\nok" {
		t.Fatalf("out: %q", out)
	}
}

func TestObserveQuotaMergesWindows(t *testing.T) {
	lt := NewLimitTracker()
	p := func(v float64) *float64 { return &v }
	now := time.Unix(1000, 0)
	lt.ObserveQuota("claude", Quota{Windows: []QuotaWindow{{Name: "five_hour", UsedPercent: p(50)}}}, now)
	lt.ObserveQuota("claude", Quota{Windows: []QuotaWindow{{Name: "seven_day", UsedPercent: p(20)}}, Plan: "max"}, now)
	lt.ObserveQuota("claude", Quota{Windows: []QuotaWindow{{Name: "five_hour", Status: "rejected"}}}, now)
	rl := lt.Snapshot()["claude"]
	if len(rl.Windows) != 2 || rl.Plan != "max" || !rl.UpdatedAt.Equal(now) {
		t.Fatalf("rl: %+v", rl)
	}
	if w := rl.Windows[0]; w.Name != "five_hour" || w.Status != "rejected" || *w.UsedPercent != 50 {
		t.Fatalf("five_hour: %+v", w)
	}
}

// fakeCodex печатает JSON-события как codex exec --json; при "limit" — исчерпанную квоту.
const fakeCodex = `#!/bin/sh
for a; do prompt="$a"; done
case "$prompt" in
  limit*) echo '{"type":"token_count","rate_limits":{"primary":{"used_percent":100,"window_minutes":300,"resets_in_seconds":120}}}'
          echo '{"type":"turn.failed","error":{"message":"usage limit reached"}}'; exit 1 ;;
esac
echo '{"type":"token_count","rate_limits":{"plan_type":"pro","primary":{"used_percent":7,"window_minutes":300,"resets_in_seconds":3600}}}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"Привет"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":11,"output_tokens":2}}'
`

func setupCodex(t *testing.T) *Server {
	t.Helper()
	script := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(script, []byte(fakeCodex), 0o755); err != nil {
		t.Fatal(err)
	}
	st := newTestStore(t)
	if _, err := st.AddProvider(store.Provider{Name: "codex", Kind: store.KindCLI, Command: []string{script, "exec", "--json", "{prompt}"}}); err != nil {
		t.Fatal(err)
	}
	return NewServer(st, testToken, nil)
}

func TestCLIQuotaThroughServer(t *testing.T) {
	srv := setupCodex(t)
	rec := serve(srv, newAuthedRequest(http.MethodPost, "/v1/chat/completions", `{"model":"codex/gpt","messages":[{"role":"user","content":"привет"}]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body)
	}
	var c Completion
	json.Unmarshal(rec.Body.Bytes(), &c)
	if got := *c.Choices[0].Message.Content; got != "Привет" {
		t.Fatalf("content = %q", got)
	}
	if c.Usage.PromptTokens != 11 || c.Usage.CompletionTokens != 2 {
		t.Fatalf("usage = %+v", c.Usage)
	}
	rl := srv.limits.Snapshot()["codex"]
	if rl.Plan != "pro" || len(rl.Windows) != 1 || *rl.Windows[0].UsedPercent != 7 {
		t.Fatalf("limits = %+v", rl)
	}
}

func TestCLIQuotaLimitGives429(t *testing.T) {
	srv := setupCodex(t)
	rec := serve(srv, newAuthedRequest(http.MethodPost, "/v1/chat/completions", `{"model":"codex/gpt","messages":[{"role":"user","content":"limit"}]}`))
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "usage limit reached") {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body)
	}
}
