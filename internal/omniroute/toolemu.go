package omniroute

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// toolEmuAdapter даёт tools провайдерам, которые умеют только текст (CLI-утилиты
// и веб-чаты). Описание инструментов вставляется в промпт, а ответ модели
// разбирается обратно в tool_calls формата OpenAI. Для клиента такой провайдер
// выглядит как обычный API с поддержкой инструментов.
type toolEmuAdapter struct {
	inner Adapter
}

const toolProtocol = `Ты работаешь как агент и можешь вызывать инструменты.

Чтобы вызвать инструмент, напиши в ответе блок ровно в таком виде:
<tool_call>{"name": "имя_инструмента", "arguments": {...}}</tool_call>

Правила:
- Внутри блока — только JSON-объект с полями "name" и "arguments".
- Можно написать несколько блоков подряд, они выполнятся по порядку.
- После блоков ничего не пиши: дождись результатов, они придут следующим сообщением.
- Сам ничего не выполняй и не выдумывай результаты инструментов.
- Если инструменты больше не нужны, ответь обычным текстом без блоков.

Доступные инструменты:
`

func (a *toolEmuAdapter) Stream(ctx context.Context, call Call, emit func(Event) error) (Usage, error) {
	inner := call
	inner.Request = withEmulatedTools(call.Request)

	var text strings.Builder
	usage, err := a.inner.Stream(ctx, inner, func(ev Event) error {
		text.WriteString(ev.Delta.Content)
		return nil
	})
	if err != nil {
		return usage, err
	}

	content, calls := parseToolCalls(text.String(), call.Request.Tools)
	if err := emit(Event{Delta: Delta{Role: "assistant", Content: content}}); err != nil {
		return usage, err
	}
	if len(calls) == 0 {
		return usage, emit(Event{FinishReason: "stop"})
	}
	for i := range calls {
		idx := i
		calls[i].Index = &idx
	}
	if err := emit(Event{Delta: Delta{ToolCalls: calls}}); err != nil {
		return usage, err
	}
	return usage, emit(Event{FinishReason: "tool_calls"})
}

// withEmulatedTools переписывает запрос для текстового провайдера: описание
// инструментов — в системное сообщение, прошлые вызовы и результаты — в текст.
func withEmulatedTools(req *ChatRequest) *ChatRequest {
	var sys strings.Builder
	sys.WriteString(toolProtocol)
	for _, t := range req.Tools {
		params := string(t.Function.Parameters)
		if params == "" {
			params = "{}"
		}
		fmt.Fprintf(&sys, "\n- %s: %s\n  Параметры (JSON Schema): %s\n", t.Function.Name, t.Function.Description, params)
	}

	out := &ChatRequest{Model: req.Model, Stream: req.Stream, MaxTokens: req.MaxTokens, MaxCompletionTokens: req.MaxCompletionTokens}
	out.Messages = append(out.Messages, textMessage("system", sys.String()))
	for _, m := range req.Messages {
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			text, _ := m.Text()
			var b strings.Builder
			b.WriteString(text)
			for _, tc := range m.ToolCalls {
				args := tc.Function.Arguments
				if !json.Valid([]byte(args)) {
					args = "{}"
				}
				fmt.Fprintf(&b, "\n<tool_call>{\"name\": %q, \"arguments\": %s}</tool_call>", tc.Function.Name, args)
			}
			out.Messages = append(out.Messages, textMessage("assistant", strings.TrimSpace(b.String())))
		case m.Role == "tool":
			text, _ := m.Text()
			out.Messages = append(out.Messages, textMessage("tool", fmt.Sprintf("[%s]\n%s", m.ToolCallID, text)))
		default:
			out.Messages = append(out.Messages, m)
		}
	}
	return out
}

func textMessage(role, text string) Message {
	b, _ := json.Marshal(text)
	return Message{Role: role, Content: b}
}

var toolCallRe = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*</tool_call>`)

// parseToolCalls вынимает из ответа модели блоки <tool_call>. Блоки с
// неизвестным инструментом или неверным JSON остаются в тексте как есть:
// агент увидит их и сможет исправиться.
func parseToolCalls(text string, tools []Tool) (string, []ToolCall) {
	known := map[string]bool{}
	for _, t := range tools {
		known[t.Function.Name] = true
	}
	var calls []ToolCall
	rest := toolCallRe.ReplaceAllStringFunc(text, func(block string) string {
		body := toolCallRe.FindStringSubmatch(block)[1]
		body = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(body, "```json"), "```"), "```"))
		var c struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal([]byte(body), &c) != nil || !known[c.Name] {
			return block
		}
		args := string(c.Arguments)
		if args == "" || args == "null" {
			args = "{}"
		}
		calls = append(calls, ToolCall{ID: newCallID(), Type: "function", Function: FunctionCall{Name: c.Name, Arguments: args}})
		return ""
	})
	return strings.TrimSpace(rest), calls
}

func newCallID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "call_" + hex.EncodeToString(b)
}
