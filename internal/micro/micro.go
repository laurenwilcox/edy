// Package micro — микро-агенты Edy: разовая узкая задача на дешёвой модели
// со строгим JSON-ответом. Ответ проверяется по схеме полей; невалидный ответ
// возвращается модели с описанием ошибки, после повторных неудач — ошибка.
package micro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/laurenwilcox/name/internal/agent"
	"github.com/laurenwilcox/name/internal/store"
)

// Типы полей схемы.
const (
	TypeString = "string"
	TypeNumber = "number"
	TypeInt    = "integer"
	TypeBool   = "boolean"
	TypeList   = "array" // список строк
)

// Field — одно поле ответа.
type Field struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Enum        []string `json:"enum,omitempty"` // допустимые значения строки
	Min         *float64 `json:"min,omitempty"`  // границы числа
	Max         *float64 `json:"max,omitempty"`
}

// Task — разовая задача микро-агента.
type Task struct {
	Name   string  // короткое имя для журнала агентов (например, "ui-bot")
	Prompt string  // что сделать
	Input  string  // данные для задачи
	Fields []Field // схема ответа
}

// Result — проверенный ответ.
type Result struct {
	Model    string         `json:"model"`
	Data     map[string]any `json:"data"`
	Attempts int            `json:"attempts"`
}

// Attempts — сколько раз модель может ответить, считая первый ответ.
const Attempts = 3

// ErrNoModel — нет подходящей модели.
var ErrNoModel = errors.New("нет модели для микро-агента: подключите провайдера")

// Journal — журнал агентов (store.Store); nil — без записи.
type Journal interface {
	CreateAgent(a store.Agent) error
	AddAgentEvent(id, kind, text string, at time.Time) error
	FinishAgent(id, status, summary, errText string) error
}

// Cheapest выбирает модель для микро-агентов: самую дешёвую по цене
// (вход + выход за 1M токенов). API- и CLI-провайдеры важнее веб-чатов:
// веб-чат медленный и хрупкий для разовых задач. Нулевая цена — бесплатная
// (локальная) модель, она выигрывает.
func Cheapest(providers []store.Provider) string {
	best, bestScore := "", math.Inf(1)
	for _, p := range providers {
		if len(p.Models) == 0 {
			continue
		}
		score := p.PriceIn + p.PriceOut
		if p.Kind == store.KindWeb {
			score += 1e6
		}
		if score < bestScore {
			best, bestScore = p.Name+"/"+p.Models[0], score
		}
	}
	return best
}

// Run выполняет задачу на модели model и возвращает проверенный JSON.
func Run(ctx context.Context, llm agent.LLM, journal Journal, model string, t Task) (Result, error) {
	if model == "" {
		return Result{}, ErrNoModel
	}
	if err := checkSchema(t.Fields); err != nil {
		return Result{}, err
	}
	id := fmt.Sprintf("micro-%s-%d", t.Name, time.Now().UnixNano())
	if journal != nil {
		journal.CreateAgent(store.Agent{ID: id, Role: store.RoleMicro, Model: model, Goal: "Микро-агент: " + t.Name, Created: time.Now()})
	}
	finish := func(status, summary, errText string) {
		if journal != nil {
			journal.FinishAgent(id, status, summary, errText)
		}
	}
	msgs := []agent.Msg{{Role: "system", Content: systemPrompt(t.Fields)}, {Role: "user", Content: userPrompt(t)}}
	var lastErr error
	for i := 1; i <= Attempts; i++ {
		reply, err := llm.Chat(ctx, model, msgs, nil)
		if err != nil {
			finish(store.AgentFailed, "", err.Error())
			return Result{Model: model, Attempts: i}, err
		}
		if journal != nil {
			journal.AddAgentEvent(id, "reply", reply.Content, time.Now())
		}
		data, err := Validate(reply.Content, t.Fields)
		if err == nil {
			finish(store.AgentDone, "Ответ принят", "")
			return Result{Model: model, Data: data, Attempts: i}, nil
		}
		lastErr = err
		msgs = append(msgs, agent.Msg{Role: "assistant", Content: reply.Content},
			agent.Msg{Role: "user", Content: "Ответ не принят: " + err.Error() + ". Пришли исправленный ответ — только JSON-объект, без пояснений."})
	}
	err := fmt.Errorf("модель %s не дала корректный JSON за %d попытки: %w", model, Attempts, lastErr)
	finish(store.AgentFailed, "", err.Error())
	return Result{Model: model, Attempts: Attempts}, err
}

func systemPrompt(fields []Field) string {
	var b strings.Builder
	b.WriteString("Ты — микро-агент Edy. Выполни одну задачу и ответь ТОЛЬКО одним JSON-объектом, без текста вокруг и без markdown.\n")
	b.WriteString("Поля объекта (других полей не добавляй):\n")
	for _, f := range fields {
		fmt.Fprintf(&b, "- %q: %s", f.Name, f.Type)
		if f.Required {
			b.WriteString(", обязательное")
		} else {
			b.WriteString(", необязательное — пропусти, если не нужно")
		}
		if len(f.Enum) > 0 {
			fmt.Fprintf(&b, ", одно из: %s", strings.Join(f.Enum, ", "))
		}
		if f.Min != nil {
			fmt.Fprintf(&b, ", не меньше %g", *f.Min)
		}
		if f.Max != nil {
			fmt.Fprintf(&b, ", не больше %g", *f.Max)
		}
		if f.Description != "" {
			b.WriteString(" — " + f.Description)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func userPrompt(t Task) string {
	if strings.TrimSpace(t.Input) == "" {
		return t.Prompt
	}
	return t.Prompt + "\n\nДанные:\n" + t.Input
}

func checkSchema(fields []Field) error {
	if len(fields) == 0 {
		return errors.New("схема ответа пуста")
	}
	seen := map[string]bool{}
	for _, f := range fields {
		if f.Name == "" || seen[f.Name] {
			return fmt.Errorf("поле %q: пустое или повторяется", f.Name)
		}
		seen[f.Name] = true
		switch f.Type {
		case TypeString, TypeNumber, TypeInt, TypeBool, TypeList:
		default:
			return fmt.Errorf("поле %q: неизвестный тип %q", f.Name, f.Type)
		}
	}
	return nil
}

// Validate достаёт JSON-объект из ответа модели и проверяет его по схеме.
// Обёртку ```json … ``` и текст вокруг объекта прощает, всё остальное — нет:
// лишние поля, неверные типы, значения вне enum и границ.
func Validate(text string, fields []Field) (map[string]any, error) {
	raw := extract(text)
	if raw == "" {
		return nil, errors.New("в ответе нет JSON-объекта")
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, fmt.Errorf("некорректный JSON: %v", err)
	}
	byName := map[string]Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	out := map[string]any{}
	for k, v := range obj {
		f, ok := byName[k]
		if !ok {
			return nil, fmt.Errorf("лишнее поле %q", k)
		}
		val, err := checkValue(f, v)
		if err != nil {
			return nil, err
		}
		out[k] = val
	}
	for _, f := range fields {
		if _, ok := out[f.Name]; f.Required && !ok {
			return nil, fmt.Errorf("нет обязательного поля %q", f.Name)
		}
	}
	return out, nil
}

func checkValue(f Field, v any) (any, error) {
	bad := func(want string) error { return fmt.Errorf("поле %q: ожидается %s", f.Name, want) }
	switch f.Type {
	case TypeString:
		s, ok := v.(string)
		if !ok {
			return nil, bad("строка")
		}
		if len(f.Enum) > 0 && !contains(f.Enum, s) {
			return nil, fmt.Errorf("поле %q: %q не из списка %s", f.Name, s, strings.Join(f.Enum, ", "))
		}
		return s, nil
	case TypeBool:
		b, ok := v.(bool)
		if !ok {
			return nil, bad("true или false")
		}
		return b, nil
	case TypeNumber, TypeInt:
		n, ok := v.(json.Number)
		if !ok {
			return nil, bad("число")
		}
		x, err := n.Float64()
		if err != nil {
			return nil, bad("число")
		}
		if f.Type == TypeInt && x != math.Trunc(x) {
			return nil, bad("целое число")
		}
		if (f.Min != nil && x < *f.Min) || (f.Max != nil && x > *f.Max) {
			return nil, fmt.Errorf("поле %q: %g вне допустимых границ", f.Name, x)
		}
		if f.Type == TypeInt {
			return int64(x), nil
		}
		return x, nil
	case TypeList:
		arr, ok := v.([]any)
		if !ok {
			return nil, bad("список строк")
		}
		out := make([]string, 0, len(arr))
		for _, it := range arr {
			s, ok := it.(string)
			if !ok {
				return nil, bad("список строк")
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, fmt.Errorf("поле %q: неизвестный тип", f.Name)
}

// extract вырезает первый сбалансированный JSON-объект из текста.
func extract(text string) string {
	start := strings.IndexByte(text, '{')
	if start < 0 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(text); i++ {
		c := text[i]
		switch {
		case esc:
			esc = false
		case inStr && c == '\\':
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return text[start : i+1]
			}
		}
	}
	return ""
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
