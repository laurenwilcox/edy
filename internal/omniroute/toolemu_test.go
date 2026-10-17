package omniroute

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/laurenwilcox/name/internal/store"
	"github.com/laurenwilcox/name/internal/webchat"
)

func TestParseToolCalls(t *testing.T) {
	tools := []Tool{{Type: "function", Function: ToolFunction{Name: "read_file"}}, {Type: "function", Function: ToolFunction{Name: "write_file"}}}
	text := "Сначала прочитаю файл.\n<tool_call>{\"name\": \"read_file\", \"arguments\": {\"path\": \"main.go\"}}</tool_call>\n" +
		"<tool_call>\n```json\n{\"name\":\"write_file\",\"arguments\":{\"path\":\"a\",\"content\":\"x\"}}\n```\n</tool_call>" +
		"<tool_call>{\"name\":\"rm_rf\",\"arguments\":{}}</tool_call>"
	content, calls := parseToolCalls(text, tools)
	if len(calls) != 2 || calls[0].Function.Name != "read_file" || calls[1].Function.Arguments != `{"path":"a","content":"x"}` {
		t.Fatalf("calls = %+v", calls)
	}
	if !strings.HasPrefix(content, "Сначала прочитаю файл.") || !strings.Contains(content, "rm_rf") {
		t.Fatalf("неизвестный инструмент должен остаться в тексте: %q", content)
	}
	if calls[0].ID == calls[1].ID || !strings.HasPrefix(calls[0].ID, "call_") {
		t.Fatalf("ids = %q %q", calls[0].ID, calls[1].ID)
	}
}

func TestWithEmulatedToolsRendersHistory(t *testing.T) {
	req := &ChatRequest{
		Tools: []Tool{{Type: "function", Function: ToolFunction{Name: "read_file", Description: "Прочитать файл", Parameters: json.RawMessage(`{"type":"object"}`)}}},
		Messages: []Message{
			textMessage("user", "что в main.go?"),
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1", Function: FunctionCall{Name: "read_file", Arguments: `{"path":"main.go"}`}}}},
			{Role: "tool", ToolCallID: "call_1", Content: json.RawMessage(`"package main"`)},
		},
	}
	out := withEmulatedTools(req)
	if len(out.Tools) != 0 || out.Messages[0].Role != "system" {
		t.Fatalf("out = %+v", out)
	}
	sys, _ := out.Messages[0].Text()
	if !strings.Contains(sys, "read_file: Прочитать файл") || !strings.Contains(sys, "<tool_call>") {
		t.Fatalf("system = %q", sys)
	}
	asst, _ := out.Messages[2].Text()
	if asst != `<tool_call>{"name": "read_file", "arguments": {"path":"main.go"}}</tool_call>` {
		t.Fatalf("assistant = %q", asst)
	}
	tool, _ := out.Messages[3].Text()
	if tool != "[call_1]\npackage main" {
		t.Fatalf("tool = %q", tool)
	}
}

// Через сервер: CLI-провайдер «отвечает» блоком <tool_call>, клиент получает tool_calls.
func TestToolEmulationThroughServer(t *testing.T) {
	st := newTestStore(t)
	st.AddProvider(store.Provider{Name: "gpt", Kind: store.KindWeb, Site: "chatgpt", Browser: "chromium"})
	srv := NewServer(st, testToken, nil)
	var gotPrompt string
	srv.RegisterAdapter(store.KindWeb, &WebAdapter{Browser: promptSpy{&gotPrompt, `Ок.<tool_call>{"name":"read_file","arguments":{"path":"go.mod"}}</tool_call>`}})

	body := `{"model":"gpt/chat","messages":[{"role":"user","content":"прочитай go.mod"}],
		"tools":[{"type":"function","function":{"name":"read_file","description":"Прочитать файл","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
	rec := serve(srv, newAuthedRequest(http.MethodPost, "/v1/chat/completions", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body)
	}
	var c Completion
	json.Unmarshal(rec.Body.Bytes(), &c)
	ch := c.Choices[0]
	if ch.FinishReason != "tool_calls" || len(ch.Message.ToolCalls) != 1 || ch.Message.ToolCalls[0].Function.Arguments != `{"path":"go.mod"}` {
		t.Fatalf("choice = %+v", ch)
	}
	if *ch.Message.Content != "Ок." {
		t.Fatalf("content = %q", *ch.Message.Content)
	}
	if !strings.Contains(gotPrompt, "read_file: Прочитать файл") {
		t.Fatalf("описание инструментов не попало в промпт: %q", gotPrompt)
	}
}

type promptSpy struct {
	got    *string
	answer string
}

func (p promptSpy) Ask(ctx context.Context, c webchat.Conn, prompt string, onText func(string) error) error {
	*p.got = prompt
	return onText(p.answer)
}
