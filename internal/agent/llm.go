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
	body, err := json.Marshal(map[string]any{"model": model, "messages": msgs, "tools": tools})
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
