package webchat

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Site описывает, как общаться с конкретным веб-чатом. Селекторы — CSS.
// Сайты часто меняют вёрстку, поэтому любой сайт можно переопределить
// в файле web-sites.json в папке данных Edy без пересборки программы.
type Site struct {
	Title string `json:"title"`
	// URL открывает новый пустой чат.
	URL string `json:"url"`
	// Input — поле ввода (textarea или contenteditable).
	Input string `json:"input"`
	// Send — кнопка отправки; если пусто, отправка клавишей Enter.
	Send string `json:"send,omitempty"`
	// Response — блок ответа модели; берётся последний на странице.
	Response string `json:"response"`
	// Busy — элемент, видимый, пока модель генерирует ответ (обычно кнопка «Стоп»).
	Busy string `json:"busy,omitempty"`

	// Вежливость: пауза между запросами и дневной лимит.
	MinIntervalSec int `json:"min_interval_sec"`
	DailyLimit     int `json:"daily_limit"`
}

func (s Site) minInterval() time.Duration { return time.Duration(s.MinIntervalSec) * time.Second }

// DefaultSites — встроенные профили. Каждый селектор — список вариантов
// через запятую: сайты меняют вёрстку, и запасные варианты продлевают жизнь
// профилю. На реальных аккаунтах селекторы не проверены. При сбое Edy
// сохраняет в web-debug снимок и список полей и кнопок страницы — по нему
// селекторы правятся в web-sites.json без пересборки.
//
// Отправка не зависит только от Send: сначала нажимается Enter, кнопка
// ищется, лишь если текст остался в поле.
var DefaultSites = map[string]Site{
	"chatgpt": {
		Title:          "ChatGPT",
		URL:            "https://chatgpt.com/",
		Input:          "#prompt-textarea, div.ProseMirror[contenteditable='true'], form textarea",
		Send:           "button[data-testid='send-button'], #composer-submit-button, button[data-testid='composer-submit-button']",
		Response:       "[data-message-author-role='assistant'] .markdown, [data-message-author-role='assistant']",
		Busy:           "button[data-testid='stop-button'], button[aria-label*='Stop' i], button[aria-label*='Останов' i]",
		MinIntervalSec: 15,
		DailyLimit:     150,
	},
	"claude": {
		Title:          "Claude.ai",
		URL:            "https://claude.ai/new",
		Input:          "div.ProseMirror[contenteditable='true'], div[contenteditable='true']",
		Send:           "button[aria-label='Send message'], button[aria-label='Send Message'], button[aria-label*='Отправ' i]",
		Response:       ".font-claude-response, .font-claude-message, [data-is-streaming] .standard-markdown, [data-is-streaming]",
		Busy:           "[data-is-streaming='true'], button[aria-label*='Stop' i], button[aria-label*='Останов' i]",
		MinIntervalSec: 15,
		DailyLimit:     150,
	},
	"gemini": {
		Title:          "Gemini",
		URL:            "https://gemini.google.com/app",
		Input:          "rich-textarea .ql-editor, rich-textarea div[contenteditable='true'], div.ql-editor[contenteditable='true']",
		Send:           "button.send-button, button[aria-label*='Send' i], button[aria-label*='Отправ' i]",
		Response:       "message-content .markdown, model-response message-content, message-content",
		Busy:           "button.send-button.stop, button[aria-label*='Stop' i], button[aria-label*='Остановить' i]",
		MinIntervalSec: 15,
		DailyLimit:     150,
	},
}

// SitesFile — имя файла с пользовательскими настройками сайтов.
const SitesFile = "web-sites.json"

// LoadSites возвращает встроенные сайты, дополненные и переопределённые
// содержимым dataDir/web-sites.json.
func LoadSites(dataDir string) (map[string]Site, error) {
	sites := make(map[string]Site, len(DefaultSites))
	for k, v := range DefaultSites {
		sites[k] = v
	}
	data, err := os.ReadFile(filepath.Join(dataDir, SitesFile))
	if errors.Is(err, os.ErrNotExist) {
		return sites, nil
	} else if err != nil {
		return nil, err
	}
	var custom map[string]Site
	if err := json.Unmarshal(data, &custom); err != nil {
		return nil, fmt.Errorf("%s: %w", SitesFile, err)
	}
	for k, v := range custom {
		base := sites[k]
		sites[k] = merge(base, v)
	}
	for k, v := range sites {
		if v.URL == "" || v.Input == "" || v.Response == "" {
			return nil, fmt.Errorf("%s: у сайта %q должны быть url, input и response", SitesFile, k)
		}
	}
	return sites, nil
}

// merge переносит в base только заданные поля из override.
func merge(base, o Site) Site {
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&base.Title, o.Title)
	set(&base.URL, o.URL)
	set(&base.Input, o.Input)
	set(&base.Send, o.Send)
	set(&base.Response, o.Response)
	set(&base.Busy, o.Busy)
	if o.MinIntervalSec > 0 {
		base.MinIntervalSec = o.MinIntervalSec
	}
	if o.DailyLimit > 0 {
		base.DailyLimit = o.DailyLimit
	}
	return base
}
