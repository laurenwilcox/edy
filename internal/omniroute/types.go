package omniroute

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ChatRequest — входящий запрос в формате OpenAI /v1/chat/completions.
// Raw хранит исходное тело целиком, чтобы OpenAI-совместимые провайдеры
// получали все поля (temperature, response_format и т.д.) без потерь.
type ChatRequest struct {
	Model               string          `json:"model"`
	Messages            []Message       `json:"messages"`
	Stream              bool            `json:"stream"`
	MaxTokens           *int64          `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int64          `json:"max_completion_tokens,omitempty"`
	Tools               []Tool          `json:"tools,omitempty"`
	Stop                json.RawMessage `json:"stop,omitempty"`

	Raw map[string]json.RawMessage `json:"-"`
}

func parseChatRequest(body []byte) (*ChatRequest, error) {
	var req ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("некорректный JSON: %w", err)
	}
	if err := json.Unmarshal(body, &req.Raw); err != nil {
		return nil, err
	}
	if req.Model == "" {
		return nil, errors.New("поле model обязательно")
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("поле messages не может быть пустым")
	}
	return &req, nil
}

// splitModel разбирает "провайдер/модель". Сама модель может содержать "/",
// например "openrouter/meta-llama/llama-3-70b".
func splitModel(model string) (provider, name string, ok bool) {
	provider, name, ok = strings.Cut(model, "/")
	return provider, name, ok && provider != "" && name != ""
}

type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type ContentPart struct {
	Type       string      `json:"type"`
	Text       string      `json:"text,omitempty"`
	ImageURL   *ImageURL   `json:"image_url,omitempty"`
	InputAudio *InputAudio `json:"input_audio,omitempty"`
}

// InputAudio — звук в сообщении (формат OpenAI): base64 и формат wav/mp3.
type InputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

type ImageURL struct {
	URL string `json:"url"`
}

// Parts возвращает содержимое сообщения как список частей: строка
// превращается в одну текстовую часть.
func (m Message) Parts() ([]ContentPart, error) {
	if len(m.Content) == 0 || string(m.Content) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return []ContentPart{{Type: "text", Text: s}}, nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return nil, fmt.Errorf("сообщение %q: content должен быть строкой или массивом частей", m.Role)
	}
	return parts, nil
}

// Text склеивает все текстовые части сообщения.
func (m Message) Text() (string, error) {
	parts, err := m.Parts()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String(), nil
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type ToolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// Delta — приращение ответа модели, которое адаптер отдаёт роутеру.
type Delta struct {
	Role      string     `json:"role,omitempty"`
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// Chunk — элемент SSE-потока chat.completion.chunk.
type Chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

// Completion — нестриминговый ответ chat.completion.
type Completion struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []CompletionChoice `json:"choices"`
	Usage   Usage              `json:"usage"`
}

type CompletionChoice struct {
	Index        int               `json:"index"`
	Message      CompletionMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

type CompletionMessage struct {
	Role      string     `json:"role"`
	Content   *string    `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}
