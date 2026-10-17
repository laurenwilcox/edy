package omniroute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/laurenwilcox/name/internal/store"
)

// ListModels спрашивает у провайдера список доступных моделей.
// Поддерживаются API-провайдеры; у CLI-утилит общего способа нет.
func ListModels(ctx context.Context, p store.Provider, key string) ([]string, error) {
	var ids []string
	var err error
	switch p.Kind {
	case store.KindOpenAI:
		ids, err = listOpenAIModels(ctx, p, key)
	case store.KindAnthropic:
		ids, err = listAnthropicModels(ctx, p, key)
	default:
		return nil, errors.New("список моделей можно загрузить только у API-провайдеров")
	}
	if err != nil {
		return nil, err
	}
	sort.Strings(ids)
	return ids, nil
}

func listOpenAIModels(ctx context.Context, p store.Provider, key string) ([]string, error) {
	base := p.BaseURL
	if base == "" {
		base = defaultOpenAIBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode/100 != 2 {
		return nil, &UpstreamError{Status: resp.StatusCode, Message: strings.TrimSpace(string(body))}
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("провайдер вернул неожиданный ответ: %w", err)
	}
	ids := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		// Google отдаёт имена с префиксом "models/".
		ids = append(ids, strings.TrimPrefix(m.ID, "models/"))
	}
	return ids, nil
}

func listAnthropicModels(ctx context.Context, p store.Provider, key string) ([]string, error) {
	opts := []option.RequestOption{option.WithAPIKey(key), option.WithMaxRetries(0)}
	if p.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(p.BaseURL))
	}
	client := anthropic.NewClient(opts...)
	pager := client.Models.ListAutoPaging(ctx, anthropic.ModelListParams{Limit: anthropic.Int(1000)})
	var ids []string
	for pager.Next() {
		ids = append(ids, pager.Current().ID)
	}
	if err := pager.Err(); err != nil {
		var apierr *anthropic.Error
		if errors.As(err, &apierr) {
			return nil, &UpstreamError{Status: apierr.StatusCode, Message: err.Error()}
		}
		return nil, err
	}
	return ids, nil
}
