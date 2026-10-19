package omniroute

import (
	"net/http"
	"regexp"
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
