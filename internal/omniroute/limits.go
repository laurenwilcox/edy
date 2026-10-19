package omniroute

import (
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RateLimit — состояние лимитов одного провайдера (или его ключа).
type RateLimit struct {
	ReqLimit      int64     `json:"req_limit,omitempty"`
	ReqRemaining  int64     `json:"req_remaining,omitempty"`
	ReqReset      time.Time `json:"req_reset,omitempty"`
	TokLimit      int64     `json:"tok_limit,omitempty"`
	TokRemaining  int64     `json:"tok_remaining,omitempty"`
	TokReset      time.Time `json:"tok_reset,omitempty"`
	LastStatus    int       `json:"last_status,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
	// Квоты подписки CLI из JSON-вывода (codex --json, claude stream-json).
	Windows []QuotaWindow `json:"quota_windows,omitempty"`
	Plan    string        `json:"plan,omitempty"`
	Credits string        `json:"credits,omitempty"`
}

// LimitTracker хранит последние известные лимиты по провайдерам.
type LimitTracker struct {
	mu sync.Mutex
	m  map[string]RateLimit
}

func NewLimitTracker() *LimitTracker { return &LimitTracker{m: map[string]RateLimit{}} }

// Observe обновляет лимиты провайдера по заголовкам ответа (OpenAI или Anthropic).
func (t *LimitTracker) Observe(provider string, h http.Header, status int, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rl := t.m[provider]
	parseLimitHeaders(&rl, h, now)
	rl.LastStatus, rl.UpdatedAt = status, now
	if status < 300 {
		rl.LastError = ""
	}
	t.m[provider] = rl
}

// Fail отмечает ошибку лимита и перерыв до until.
func (t *LimitTracker) Fail(provider string, status int, msg string, until, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rl := t.m[provider]
	rl.LastStatus, rl.LastError, rl.UpdatedAt = status, msg, now
	if until.After(rl.CooldownUntil) {
		rl.CooldownUntil = until
	}
	t.m[provider] = rl
}

// ObserveQuota запоминает квоты подписки CLI. Окна сливаются по имени:
// claude присылает их по одному событию на окно.
func (t *LimitTracker) ObserveQuota(provider string, q Quota, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rl := t.m[provider]
	// Новый срез, а не правка старого: Snapshot отдаёт его наружу без копии.
	windows := append([]QuotaWindow(nil), rl.Windows...)
	for _, w := range q.Windows {
		i := slices.IndexFunc(windows, func(o QuotaWindow) bool { return o.Name == w.Name })
		if i < 0 {
			windows = append(windows, w)
			continue
		}
		if w.UsedPercent == nil { // claude не всегда сообщает процент — оставляем прежний
			w.UsedPercent = windows[i].UsedPercent
		}
		windows[i] = w
	}
	rl.Windows, rl.UpdatedAt = windows, now
	if q.Plan != "" {
		rl.Plan = q.Plan
	}
	if q.Credits != "" {
		rl.Credits = q.Credits
	}
	t.m[provider] = rl
}

// Snapshot возвращает копию всех известных лимитов.
func (t *LimitTracker) Snapshot() map[string]RateLimit {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]RateLimit, len(t.m))
	for k, v := range t.m {
		out[k] = v
	}
	return out
}

func parseLimitHeaders(rl *RateLimit, h http.Header, now time.Time) {
	num := func(names ...string) (int64, bool) {
		for _, n := range names {
			if v := h.Get(n); v != "" {
				if x, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
					return x, true
				}
			}
		}
		return 0, false
	}
	reset := func(names ...string) (time.Time, bool) {
		for _, n := range names {
			if v := h.Get(n); v != "" {
				if ts, ok := parseReset(v, now); ok {
					return ts, true
				}
			}
		}
		return time.Time{}, false
	}
	if v, ok := num("x-ratelimit-limit-requests", "anthropic-ratelimit-requests-limit"); ok {
		rl.ReqLimit = v
	}
	if v, ok := num("x-ratelimit-remaining-requests", "anthropic-ratelimit-requests-remaining"); ok {
		rl.ReqRemaining = v
	}
	if v, ok := reset("x-ratelimit-reset-requests", "anthropic-ratelimit-requests-reset"); ok {
		rl.ReqReset = v
	}
	if v, ok := num("x-ratelimit-limit-tokens", "anthropic-ratelimit-tokens-limit"); ok {
		rl.TokLimit = v
	}
	if v, ok := num("x-ratelimit-remaining-tokens", "anthropic-ratelimit-tokens-remaining"); ok {
		rl.TokRemaining = v
	}
	if v, ok := reset("x-ratelimit-reset-tokens", "anthropic-ratelimit-tokens-reset"); ok {
		rl.TokReset = v
	}
}

// parseReset понимает длительность OpenAI ("6m0s", "20ms"), секунды и RFC3339 (Anthropic).
func parseReset(v string, now time.Time) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if ts, err := time.Parse(time.RFC3339, v); err == nil {
		return ts, true
	}
	if d, err := time.ParseDuration(v); err == nil {
		return now.Add(d), true
	}
	if s, err := strconv.ParseFloat(v, 64); err == nil {
		return now.Add(time.Duration(s * float64(time.Second))), true
	}
	return time.Time{}, false
}

var (
	cliLimitRe = regexp.MustCompile(`(?i)(usage limit|rate limit|limit reached|quota|resource_exhausted|too many requests|\b429\b)`)
	cliRetryRe = regexp.MustCompile(`(?i)try again in\s+(\d+(?:\.\d+)?)\s*(s|sec|seconds?|m|min|minutes?|h|hours?)`)
)

// cliToolDeniedRe — CLI-агент хотел сам запустить инструмент (команду,
// правку файла), а в неинтерактивном режиме спросить разрешения некого:
// так отвечает, например, agy («headless mode cannot prompt … auto-denied»).
var cliToolDeniedRe = regexp.MustCompile(`(?i)headless mode cannot prompt|permission[^.\n]{0,80}auto-denied|auto-denied[^.\n]{0,80}permission`)

// DetectCLIToolDenied сообщает, что вместо ответа CLI вывела отказ в правах
// на свой инструмент. Повторять такой запрос бесполезно: ответ будет тем же.
func DetectCLIToolDenied(out string) bool {
	return cliToolDeniedRe.MatchString(out)
}

// modelScopedRe — в сообщении о лимите названа модель: квота своя у каждой
// модели (Google: «Quota exceeded for metric: …, limit: 0, model: gemini-…»).
var modelScopedRe = regexp.MustCompile(`(?i)\bmodel["']?\s*[:=]\s*["']?[\w.-]+`)

// ModelScopedLimit сообщает, что 429 относится к одной модели, а не ко всему ключу.
func ModelScopedLimit(msg string) bool { return modelScopedRe.MatchString(msg) }

// DeadQuota сообщает, что квота модели нулевая («limit: 0»): модель на этом
// тарифе не работает вообще, ждать сброса бесполезно.
func DeadQuota(msg string) bool { return deadQuotaRe.MatchString(msg) }

var deadQuotaRe = regexp.MustCompile(`(?i)\blimit:\s*0\b`)

// DetectCLILimit ищет в выводе CLI сообщение о лимите и, если есть, время ожидания.
func DetectCLILimit(out string) (bool, time.Duration) {
	if !cliLimitRe.MatchString(out) {
		return false, 0
	}
	m := cliRetryRe.FindStringSubmatch(out)
	if m == nil {
		return true, 0
	}
	n, _ := strconv.ParseFloat(m[1], 64)
	unit := time.Second
	switch strings.ToLower(m[2])[0] {
	case 'm':
		unit = time.Minute
	case 'h':
		unit = time.Hour
	}
	return true, time.Duration(n * float64(unit))
}
