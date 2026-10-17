package omniroute

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/laurenwilcox/name/internal/webchat"
)

// WebAsker — то, что адаптеру нужно от менеджера браузеров (webchat.Manager).
type WebAsker interface {
	Ask(ctx context.Context, c webchat.Conn, prompt string, onText func(full string) error) error
}

// WebAdapter общается с веб-чатами через браузер. Сайт отвечает только
// текстом, поэтому tools и картинки пока не поддерживаются.
type WebAdapter struct {
	Browser WebAsker
}

func (a *WebAdapter) Stream(ctx context.Context, call Call, emit func(Event) error) (Usage, error) {
	if call.Provider.Site == "" {
		return Usage{}, &UpstreamError{Status: http.StatusInternalServerError, Message: "у веб-провайдера не указан сайт"}
	}
	if len(call.Request.Tools) > 0 {
		return Usage{}, &UpstreamError{Status: http.StatusBadRequest, Message: "веб-чаты не поддерживают tools"}
	}
	prompt, err := renderPrompt(call.Request.Messages)
	if err != nil {
		return Usage{}, &UpstreamError{Status: http.StatusBadRequest, Message: err.Error()}
	}
	if err := emit(Event{Delta: Delta{Role: "assistant"}}); err != nil {
		return Usage{}, err
	}
	sent := ""
	// Профиль браузера — имя провайдера: у каждого подключения своё окружение.
	conn := webchat.Conn{Profile: call.Provider.Name, Site: call.Provider.Site, Browser: call.Provider.Browser}
	err = a.Browser.Ask(ctx, conn, prompt, func(full string) error {
		// Отдаём только дописанный хвост. Если сайт переписал начало ответа
		// (бывает при отрисовке разметки), уже отправленное не трогаем.
		if !strings.HasPrefix(full, sent) {
			return nil
		}
		delta := full[len(sent):]
		sent = full
		if delta == "" {
			return nil
		}
		return emit(Event{Delta: Delta{Content: delta}})
	})
	usage := Usage{PromptTokens: estimateTokens(prompt), CompletionTokens: estimateTokens(sent)}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return usage, err
		}
		return usage, &UpstreamError{Status: http.StatusBadGateway, Message: err.Error()}
	}
	return usage, emit(Event{FinishReason: "stop"})
}
