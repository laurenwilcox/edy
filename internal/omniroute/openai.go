package omniroute

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const defaultOpenAIBaseURL = "https://api.openai.com/v1"

// OpenAIAdapter работает с любым OpenAI-совместимым API (OpenAI, OpenRouter,
// DeepSeek, Groq, локальная Ollama на /v1). Тело запроса передаётся как есть,
// меняются только model и параметры стриминга.
type OpenAIAdapter struct {
	HTTP *http.Client
}

func (a *OpenAIAdapter) Stream(ctx context.Context, call Call, emit func(Event) error) (Usage, error) {
	body := make(map[string]json.RawMessage, len(call.Request.Raw)+2)
	for k, v := range call.Request.Raw {
		body[k] = v
	}
	body["model"], _ = json.Marshal(call.Model)
	body["stream"] = json.RawMessage(`true`)
	body["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	payload, err := json.Marshal(body)
	if err != nil {
		return Usage{}, err
	}

	base := call.Provider.BaseURL
	if base == "" {
		base = defaultOpenAIBaseURL
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return Usage{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if call.Key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+call.Key)
	}

	client := a.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return Usage{}, err
	}
	defer resp.Body.Close()
	call.observe(resp.StatusCode, resp.Header)

	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Usage{}, &UpstreamError{Status: resp.StatusCode, RetryAfter: retryAfter(resp.Header), Message: strings.TrimSpace(string(msg))}
	}

	var usage Usage
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		var chunk Chunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return usage, fmt.Errorf("не удалось разобрать SSE-событие провайдера: %w", err)
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		for _, c := range chunk.Choices {
			if c.Index != 0 {
				continue // OmniRoute отдаёт только первый вариант ответа
			}
			ev := Event{Delta: c.Delta}
			if c.FinishReason != nil {
				ev.FinishReason = *c.FinishReason
			}
			if err := emit(ev); err != nil {
				return usage, err
			}
		}
	}
	return usage, scanner.Err()
}
