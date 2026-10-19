package omniroute

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// QuotaWindow — одно окно квоты подписки CLI (например, 5 часов или неделя).
type QuotaWindow struct {
	Name          string    `json:"name"`
	UsedPercent   *float64  `json:"used_percent,omitempty"` // nil — CLI не сообщил процент
	WindowMinutes int       `json:"window_minutes,omitempty"`
	ResetsAt      time.Time `json:"resets_at,omitempty"`
	Status        string    `json:"status,omitempty"` // claude: allowed, allowed_warning, rejected
}

// Quota — квоты подписки, прочитанные из JSON-вывода CLI.
type Quota struct {
	Windows []QuotaWindow
	Plan    string
	Credits string
}

// jsonOutputCommand сообщает, просит ли команда у CLI JSON-вывод
// (codex exec --json, claude --output-format stream-json, gemini --output-format json).
func jsonOutputCommand(args []string) bool {
	for i, a := range args {
		switch {
		case a == "--json", a == "--output-format=json", a == "--output-format=stream-json":
			return true
		case (a == "--output-format" || a == "-o") && i+1 < len(args):
			if f := args[i+1]; f == "json" || f == "stream-json" {
				return true
			}
		}
	}
	return false
}

// cliJSONFilter разбирает построчный JSON-вывод CLI: текст ответа отдаёт
// дальше, квоты — в onQuota, служебные события проглатывает. Строки не в
// формате JSON пропускает как есть: так видны предупреждения утилиты.
type cliJSONFilter struct {
	onQuota func(Quota)
	now     func() time.Time

	line    strings.Builder // незавершённая строка
	texts   int             // сколько отдельных реплик уже отдано
	errMsg  string          // ошибка из turn.failed / result.is_error
	limited bool            // CLI прямо сообщил, что лимит исчерпан
	resetAt time.Time       // когда лимит сбросится (если известно)
	usage   Usage           // настоящие токены, если CLI их сообщил
}

// Write принимает очередной кусок вывода и возвращает текст для клиента.
func (f *cliJSONFilter) Write(text string) string {
	var out strings.Builder
	for {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			f.line.WriteString(text)
			return out.String()
		}
		f.line.WriteString(strings.TrimSuffix(text[:i], "\r"))
		out.WriteString(f.handle(f.line.String(), true))
		f.line.Reset()
		text = text[i+1:]
	}
}

// Flush дорабатывает последнюю строку без перевода строки.
func (f *cliJSONFilter) Flush() string {
	s := f.line.String()
	f.line.Reset()
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return f.handle(s, false)
}

// Pending — незавершённая строка (для диагностики зависаний).
func (f *cliJSONFilter) Pending() string { return f.line.String() }

func (f *cliJSONFilter) handle(line string, newline bool) string {
	s := strings.TrimSpace(line)
	var ev map[string]any
	if !strings.HasPrefix(s, "{") || json.Unmarshal([]byte(s), &ev) != nil {
		if newline {
			return line + "\n"
		}
		return line
	}
	if q, ok := f.quotaOf(ev); ok && f.onQuota != nil {
		f.onQuota(q)
	}
	return f.textOf(ev)
}

// reply отделяет новую реплику от предыдущей пустой строкой.
func (f *cliJSONFilter) reply(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	f.texts++
	if f.texts > 1 {
		return "\n\n" + text
	}
	return text
}

// textOf достаёт текст ответа из события codex, claude или gemini.
func (f *cliJSONFilter) textOf(ev map[string]any) string {
	typ := str(ev["type"])
	switch typ {
	// codex exec --json
	case "item.completed":
		item := obj(ev["item"])
		switch str(item["type"]) {
		case "agent_message":
			return f.reply(str(item["text"]))
		}
	case "turn.completed":
		f.addUsage(obj(ev["usage"]))
	case "turn.failed":
		f.fail(str(obj(ev["error"])["message"]))
	case "error":
		f.fail(str(ev["message"]))

	// claude -p --output-format stream-json / json
	case "assistant":
		var b strings.Builder
		for _, c := range arr(obj(ev["message"])["content"]) {
			if c := obj(c); str(c["type"]) == "text" {
				b.WriteString(str(c["text"]))
			}
		}
		return f.reply(b.String())
	case "result":
		f.addUsage(obj(ev["usage"]))
		if ev["is_error"] == true {
			f.fail(str(ev["result"]))
			return ""
		}
		if f.texts == 0 { // в формате json ответ приходит только здесь
			return f.reply(str(ev["result"]))
		}

	// gemini --output-format stream-json
	case "message":
		if str(ev["role"]) == "assistant" {
			f.texts = max(f.texts, 1) // дельты одной реплики склеиваются без разрывов
			return str(ev["content"])
		}

	case "":
		// Старый формат codex: {"msg":{"type":"agent_message","message":…}}.
		if msg := obj(ev["msg"]); str(msg["type"]) == "agent_message" {
			return f.reply(str(msg["message"]))
		}
		// gemini --output-format json: {"response": …, "stats": …}.
		if r, ok := ev["response"].(string); ok {
			return f.reply(r)
		}
		if e := obj(ev["error"]); e != nil {
			f.fail(str(e["message"]))
		}
	}
	return ""
}

func (f *cliJSONFilter) fail(msg string) {
	if msg = strings.TrimSpace(msg); msg != "" {
		f.errMsg = msg
	}
}

func (f *cliJSONFilter) addUsage(u map[string]any) {
	in, out := num(u["input_tokens"]), num(u["output_tokens"])
	if in == 0 && out == 0 {
		return
	}
	f.usage.PromptTokens += int64(in + num(u["cache_read_input_tokens"]) + num(u["cache_creation_input_tokens"]))
	f.usage.CompletionTokens += int64(out)
	f.usage.TotalTokens = f.usage.PromptTokens + f.usage.CompletionTokens
}

// quotaOf ищет квоты: rate_limit_event у claude и rate_limits (на любой
// глубине — внутри token_count) у codex.
func (f *cliJSONFilter) quotaOf(ev map[string]any) (Quota, bool) {
	if str(ev["type"]) == "rate_limit_event" {
		return f.claudeQuota(obj(ev["rate_limit_info"]))
	}
	if rl := findKey(ev, "rate_limits", 6); rl != nil {
		return f.codexQuota(rl)
	}
	return Quota{}, false
}

// claudeQuota: {"status","resetsAt","rateLimitType","utilization"}.
func (f *cliJSONFilter) claudeQuota(info map[string]any) (Quota, bool) {
	if info == nil {
		return Quota{}, false
	}
	w := QuotaWindow{Name: str(info["rateLimitType"]), Status: str(info["status"])}
	if w.Name == "" {
		w.Name = "limit"
	}
	if ts := num(info["resetsAt"]); ts > 0 {
		w.ResetsAt = time.Unix(int64(ts), 0)
	}
	if u, ok := info["utilization"].(float64); ok {
		p := u * 100
		if u > 1 { // на случай, если придёт уже в процентах
			p = u
		}
		w.UsedPercent = &p
	}
	switch w.Name {
	case "five_hour":
		w.WindowMinutes = 300
	case "seven_day", "seven_day_opus", "seven_day_sonnet":
		w.WindowMinutes = 7 * 24 * 60
	}
	if w.Status == "rejected" {
		f.limit(w.ResetsAt)
	}
	return Quota{Windows: []QuotaWindow{w}}, true
}

// codexQuota: {"primary":{used_percent,window_minutes,resets_at|resets_in_seconds},
// "secondary":{…}, "credits":{has_credits,unlimited,balance}, "plan_type"}.
func (f *cliJSONFilter) codexQuota(rl map[string]any) (Quota, bool) {
	q := Quota{Plan: str(rl["plan_type"])}
	for _, name := range []string{"primary", "secondary"} {
		raw := obj(rl[name])
		if raw == nil {
			continue
		}
		w := QuotaWindow{Name: name, WindowMinutes: int(num(raw["window_minutes"]))}
		if p, ok := raw["used_percent"].(float64); ok {
			w.UsedPercent = &p
			if p >= 100 {
				w.Status = "rejected"
			}
		}
		if ts := num(raw["resets_at"]); ts > 0 {
			w.ResetsAt = time.Unix(int64(ts), 0)
		} else if sec := num(raw["resets_in_seconds"]); sec > 0 {
			w.ResetsAt = f.now().Add(time.Duration(sec * float64(time.Second)))
		}
		if w.Status == "rejected" {
			f.limit(w.ResetsAt)
		}
		q.Windows = append(q.Windows, w)
	}
	if c := obj(rl["credits"]); c != nil {
		switch {
		case c["unlimited"] == true:
			q.Credits = "без ограничений"
		case c["balance"] != nil:
			q.Credits = fmt.Sprint(c["balance"])
		case c["has_credits"] == false:
			q.Credits = "нет"
		}
	}
	return q, len(q.Windows) > 0 || q.Plan != "" || q.Credits != ""
}

func (f *cliJSONFilter) limit(reset time.Time) {
	f.limited = true
	if reset.After(f.resetAt) {
		f.resetAt = reset
	}
}

// findKey ищет ключ на глубине не больше depth.
func findKey(v any, key string, depth int) map[string]any {
	if depth < 0 {
		return nil
	}
	switch x := v.(type) {
	case map[string]any:
		if m := obj(x[key]); m != nil {
			return m
		}
		for _, c := range x {
			if m := findKey(c, key, depth-1); m != nil {
				return m
			}
		}
	case []any:
		for _, c := range x {
			if m := findKey(c, key, depth-1); m != nil {
				return m
			}
		}
	}
	return nil
}

func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func arr(v any) []any          { a, _ := v.([]any); return a }
func str(v any) string         { s, _ := v.(string); return s }

func num(v any) float64 {
	if x, ok := v.(float64); ok && !math.IsNaN(x) {
		return x
	}
	return 0
}
