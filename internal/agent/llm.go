// Package agent — агентный цикл Edy: модель правит проект через инструменты,
// после работы запускаются хуки (линтеры и тесты), каждый шаг сохраняется
// в Shadow Git, а после трёх провалов подряд срабатывает Circuit Breaker.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
)

// Msg — сообщение в формате OpenAI chat.completions.
type Msg struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ToolDef — описание инструмента для модели.
type ToolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// CallOptions — параметры вызова для CLI-провайдеров: рабочая папка и
// MCP-сервер Edy, который нужно подключить к утилите.
type CallOptions struct {
	Workdir string
	MCP     string
	// MCPServers — имена внешних MCP-серверов из каталога Edy в дополнение к MCP Edy.
	MCPServers []string
}

type callOptionsKey struct{}

// WithCallOptions прикладывает параметры к вызовам модели в этом контексте.
func WithCallOptions(ctx context.Context, o CallOptions) context.Context {
	return context.WithValue(ctx, callOptionsKey{}, o)
}

func callOptions(ctx context.Context) CallOptions {
	o, _ := ctx.Value(callOptionsKey{}).(CallOptions)
	return o
}

// LLM — модель, к которой агент обращается за следующим шагом.
type LLM interface {
	Chat(ctx context.Context, model string, msgs []Msg, tools []ToolDef) (Msg, error)
}

// OmniRoute — клиент локального OmniRoute (формат OpenAI). Через него агент
// работает с любым провайдером: API, CLI-утилитой или веб-чатом.
type OmniRoute struct {
	BaseURL string // например http://127.0.0.1:11435/v1
	Token   string
	Client  string // подпись агента для учёта расходов (X-Edy-Client)
	HTTP    *http.Client
}

func (o *OmniRoute) Chat(ctx context.Context, model string, msgs []Msg, tools []ToolDef) (Msg, error) {
	payload := map[string]any{"model": model, "messages": msgs}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Msg{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(o.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Msg{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.Token)
	if o.Client != "" {
		req.Header.Set("X-Edy-Client", o.Client)
	}
	if opt := callOptions(ctx); opt.Workdir != "" || opt.MCP != "" {
		if opt.Workdir != "" {
			req.Header.Set("X-Edy-Workdir", opt.Workdir)
		}
		if opt.MCP != "" {
			req.Header.Set("X-Edy-MCP", opt.MCP)
			if len(opt.MCPServers) > 0 {
				req.Header.Set("X-Edy-MCP-Servers", strings.Join(opt.MCPServers, ","))
			}
		}
	}
	client := o.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return Msg{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(data, &e)
		if e.Error.Message == "" {
			e.Error.Message = strings.TrimSpace(string(data))
		}
		return Msg{}, fmt.Errorf("модель %s: %s", model, e.Error.Message)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content   *string    `json:"content"`
				ToolCalls []ToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &out); err != nil || len(out.Choices) == 0 {
		return Msg{}, fmt.Errorf("модель %s вернула неожиданный ответ", model)
	}
	m := Msg{Role: "assistant", ToolCalls: out.Choices[0].Message.ToolCalls}
	if c := out.Choices[0].Message.Content; c != nil {
		m.Content = *c
	}
	return m, nil
}

func unmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

// InProcess возвращает HTTP-клиент, который вызывает обработчик OmniRoute
// прямо в процессе, без сети.
func InProcess(h http.Handler) *http.Client {
	return &http.Client{Transport: handlerTransport{h}}
}

type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}
