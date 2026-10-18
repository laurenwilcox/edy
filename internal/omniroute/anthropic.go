package omniroute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// defaultAnthropicMaxTokens используется, когда клиент не передал max_tokens
// (в Anthropic API это поле обязательно).
const defaultAnthropicMaxTokens = 32000

// AnthropicAdapter переводит OpenAI-запрос в Anthropic Messages API и обратно.
// Sampling-параметры (temperature, top_p) не пробрасываются: новые модели
// Claude отвечают на них ошибкой 400.
type AnthropicAdapter struct {
	HTTP *http.Client
}

func (a *AnthropicAdapter) Stream(ctx context.Context, call Call, emit func(Event) error) (Usage, error) {
	params, err := toAnthropicParams(call.Model, call.Request)
	if err != nil {
		return Usage{}, &UpstreamError{Status: http.StatusBadRequest, Message: err.Error()}
	}

	opts := []option.RequestOption{
		option.WithAPIKey(call.Key),
		option.WithMaxRetries(0), // повторами и ротацией ключей управляет OmniRoute
	}
	if call.Provider.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(call.Provider.BaseURL))
	}
	if a.HTTP != nil {
		opts = append(opts, option.WithHTTPClient(a.HTTP))
	}
	client := anthropic.NewClient(opts...)

	stream := client.Messages.NewStreaming(ctx, params)
	defer stream.Close()

	var usage Usage
	toolIndex := map[int64]int{} // индекс блока Anthropic → индекс tool_call OpenAI
	started := false
	for stream.Next() {
		if !started {
			started = true
			if err := emit(Event{Delta: Delta{Role: "assistant"}}); err != nil {
				return usage, err
			}
		}
		var ev Event
		switch e := stream.Current().AsAny().(type) {
		case anthropic.MessageStartEvent:
			u := e.Message.Usage
			usage.PromptTokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
			continue
		case anthropic.ContentBlockStartEvent:
			tu, ok := e.ContentBlock.AsAny().(anthropic.ToolUseBlock)
			if !ok {
				continue
			}
			idx := len(toolIndex)
			toolIndex[e.Index] = idx
			ev.Delta.ToolCalls = []ToolCall{{Index: &idx, ID: tu.ID, Type: "function", Function: FunctionCall{Name: tu.Name}}}
		case anthropic.ContentBlockDeltaEvent:
			switch d := e.Delta.AsAny().(type) {
			case anthropic.TextDelta:
				ev.Delta.Content = d.Text
			case anthropic.InputJSONDelta:
				idx, ok := toolIndex[e.Index]
				if !ok {
					continue
				}
				ev.Delta.ToolCalls = []ToolCall{{Index: &idx, Function: FunctionCall{Arguments: d.PartialJSON}}}
			default:
				continue // thinking и прочие служебные дельты наружу не отдаём
			}
		case anthropic.MessageDeltaEvent:
			usage.CompletionTokens = e.Usage.OutputTokens
			ev.FinishReason = finishReason(string(e.Delta.StopReason))
		default:
			continue
		}
		if err := emit(ev); err != nil {
			return usage, err
		}
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens

	if err := stream.Err(); err != nil {
		var apierr *anthropic.Error
		if !started && errors.As(err, &apierr) {
			ue := &UpstreamError{Status: apierr.StatusCode, Message: err.Error()}
			if apierr.Response != nil {
				ue.RetryAfter = retryAfter(apierr.Response.Header)
			}
			return usage, ue
		}
		return usage, err
	}
	return usage, nil
}

func finishReason(stop string) string {
	switch stop {
	case "end_turn", "stop_sequence", "pause_turn":
		return "stop"
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	case "":
		return ""
	default:
		return "stop"
	}
}

func toAnthropicParams(model string, req *ChatRequest) (anthropic.MessageNewParams, error) {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: defaultAnthropicMaxTokens,
	}
	if req.MaxCompletionTokens != nil {
		params.MaxTokens = *req.MaxCompletionTokens
	} else if req.MaxTokens != nil {
		params.MaxTokens = *req.MaxTokens
	}

	if len(req.Stop) > 0 {
		var one string
		var many []string
		if json.Unmarshal(req.Stop, &one) == nil && one != "" {
			params.StopSequences = []string{one}
		} else if json.Unmarshal(req.Stop, &many) == nil {
			params.StopSequences = many
		}
	}

	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			return params, fmt.Errorf("тип инструмента %q не поддерживается", t.Type)
		}
		tool := anthropic.ToolParam{
			Name:        t.Function.Name,
			InputSchema: toInputSchema(t.Function.Parameters),
		}
		if t.Function.Description != "" {
			tool.Description = anthropic.String(t.Function.Description)
		}
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{OfTool: &tool})
	}

	var msgs []anthropic.MessageParam
	// add дописывает блоки к последнему сообщению той же роли: Anthropic
	// ожидает, что результаты нескольких инструментов придут одним сообщением.
	add := func(role anthropic.MessageParamRole, blocks ...anthropic.ContentBlockParamUnion) {
		if len(blocks) == 0 {
			return
		}
		if n := len(msgs); n > 0 && msgs[n-1].Role == role {
			msgs[n-1].Content = append(msgs[n-1].Content, blocks...)
			return
		}
		msgs = append(msgs, anthropic.MessageParam{Role: role, Content: blocks})
	}

	for _, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			text, err := m.Text()
			if err != nil {
				return params, err
			}
			if text != "" {
				params.System = append(params.System, anthropic.TextBlockParam{Text: text})
			}
		case "user":
			blocks, err := userBlocks(m)
			if err != nil {
				return params, err
			}
			add(anthropic.MessageParamRoleUser, blocks...)
		case "assistant":
			text, err := m.Text()
			if err != nil {
				return params, err
			}
			var blocks []anthropic.ContentBlockParamUnion
			if text != "" {
				blocks = append(blocks, anthropic.NewTextBlock(text))
			}
			for _, tc := range m.ToolCalls {
				input := json.RawMessage(tc.Function.Arguments)
				if !json.Valid(input) {
					input = json.RawMessage(`{}`)
				}
				blocks = append(blocks, anthropic.NewToolUseBlock(tc.ID, input, tc.Function.Name))
			}
			add(anthropic.MessageParamRoleAssistant, blocks...)
		case "tool":
			text, err := m.Text()
			if err != nil {
				return params, err
			}
			add(anthropic.MessageParamRoleUser, anthropic.NewToolResultBlock(m.ToolCallID, text, false))
		default:
			return params, fmt.Errorf("неизвестная роль сообщения %q", m.Role)
		}
	}
	if len(msgs) == 0 {
		return params, errors.New("нет сообщений пользователя")
	}
	params.Messages = msgs
	return params, nil
}

func userBlocks(m Message) ([]anthropic.ContentBlockParamUnion, error) {
	parts, err := m.Parts()
	if err != nil {
		return nil, err
	}
	var blocks []anthropic.ContentBlockParamUnion
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				blocks = append(blocks, anthropic.NewTextBlock(p.Text))
			}
		case "image_url":
			if p.ImageURL == nil {
				return nil, errors.New("image_url без url")
			}
			b, err := imageBlock(p.ImageURL.URL)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, b)
		case "input_audio":
			return nil, errors.New("Anthropic API не принимает звук — выберите модель с тегом «Звук»")
		default:
			return nil, fmt.Errorf("тип части сообщения %q не поддерживается", p.Type)
		}
	}
	return blocks, nil
}

// imageBlock принимает data:-URL (base64) или обычную http(s)-ссылку.
func imageBlock(url string) (anthropic.ContentBlockParamUnion, error) {
	rest, ok := strings.CutPrefix(url, "data:")
	if !ok {
		return anthropic.NewImageBlock(anthropic.URLImageSourceParam{URL: url}), nil
	}
	meta, data, ok := strings.Cut(rest, ",")
	mediaType, isBase64 := strings.CutSuffix(meta, ";base64")
	if !ok || !isBase64 {
		return anthropic.ContentBlockParamUnion{}, errors.New("картинка в data:-URL должна быть в base64")
	}
	return anthropic.NewImageBlockBase64(mediaType, data), nil
}

func toInputSchema(raw json.RawMessage) anthropic.ToolInputSchemaParam {
	var schema anthropic.ToolInputSchemaParam
	var m map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return schema
	}
	schema.Properties = m["properties"]
	if req, ok := m["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				schema.Required = append(schema.Required, s)
			}
		}
	}
	delete(m, "properties")
	delete(m, "required")
	delete(m, "type")
	if len(m) > 0 {
		schema.ExtraFields = m
	}
	return schema
}
