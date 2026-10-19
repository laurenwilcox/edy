package omniroute

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/laurenwilcox/name/internal/store"
)

// Event — то, что адаптер отдаёт наружу по мере генерации ответа.
// FinishReason заполняется один раз, в конце.
type Event struct {
	Delta        Delta
	FinishReason string
}

// Call — один вызов модели с конкретным ключом.
type Call struct {
	Provider store.Provider
	Key      string
	Model    string // имя модели без префикса провайдера
	Request  *ChatRequest
	Workdir  string // рабочая папка для CLI-провайдеров
	// MCP — MCP-сервер Edy, который нужно подключить к CLI-утилите (nil — не нужно).
	MCP *MCPAttach
	// OnHeaders получает заголовки ответа провайдера — из них читаются лимиты (nil — не нужно).
	OnHeaders func(status int, h http.Header)
	// OnQuota получает квоты подписки из JSON-вывода CLI (nil — не нужно).
	OnQuota func(q Quota)
}

func (c Call) observe(status int, h http.Header) {
	if c.OnHeaders != nil && h != nil {
		c.OnHeaders(status, h)
	}
}

// Adapter превращает OpenAI-запрос в обращение к конкретному типу источника ИИ.
// Ответ всегда идёт потоком через emit; нестриминговый режим собирает его в роутере.
type Adapter interface {
	Stream(ctx context.Context, call Call, emit func(Event) error) (Usage, error)
}

// UpstreamError — ответ провайдера с HTTP-ошибкой до начала генерации.
type UpstreamError struct {
	Status     int
	RetryAfter time.Duration
	Message    string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("провайдер ответил %d: %s", e.Status, e.Message)
}

func retryAfter(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
	v := h.Get("Retry-After")
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return time.Until(t)
	}
	return 0
}
