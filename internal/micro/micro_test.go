package micro

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/laurenwilcox/name/internal/agent"
	"github.com/laurenwilcox/name/internal/store"
)

// scripted отвечает заранее заданными репликами по очереди.
type scripted struct {
	replies []string
	calls   [][]agent.Msg
}

func (s *scripted) Chat(_ context.Context, _ string, msgs []agent.Msg, _ []agent.ToolDef) (agent.Msg, error) {
	s.calls = append(s.calls, msgs)
	if len(s.replies) == 0 {
		return agent.Msg{}, errors.New("нет ответа")
	}
	r := s.replies[0]
	s.replies = s.replies[1:]
	return agent.Msg{Role: "assistant", Content: r}, nil
}

func f(x float64) *float64 { return &x }

var fields = []Field{
	{Name: "theme", Type: TypeString, Enum: []string{"light", "dark"}, Required: true},
	{Name: "size", Type: TypeInt, Min: f(12), Max: f(18)},
	{Name: "tags", Type: TypeList},
	{Name: "ok", Type: TypeBool},
}

func TestValidate(t *testing.T) {
	data, err := Validate("Вот:\n```json\n{\"theme\":\"dark\",\"size\":14,\"tags\":[\"a\"],\"ok\":true}\n```", fields)
	if err != nil || data["theme"] != "dark" || data["size"] != int64(14) || data["ok"] != true {
		t.Fatalf("data=%v err=%v", data, err)
	}
	bad := map[string]string{
		"нет JSON":       "просто текст",
		"не из списка":   `{"theme":"blue"}`,
		"обязательного":  `{"size":14}`,
		"границ":         `{"theme":"dark","size":40}`,
		"целое":          `{"theme":"dark","size":13.5}`,
		"лишнее":         `{"theme":"dark","x":1}`,
		"список":         `{"theme":"dark","tags":[1]}`,
		"некорректный":   `{"theme":"dark",}`,
		"true или false": `{"theme":"dark","ok":"yes"}`,
	}
	for want, text := range bad {
		if _, err := Validate(text, fields); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err=%v", want, err)
		}
	}
	// Фигурные скобки внутри строк не ломают вырезание объекта.
	if data, err := Validate(`{"theme":"dark","tags":["}{"]} хвост }`, fields); err != nil || data["tags"].([]string)[0] != "}{" {
		t.Fatalf("data=%v err=%v", data, err)
	}
}

func TestRunRetries(t *testing.T) {
	llm := &scripted{replies: []string{"не знаю", `{"theme":"light"}`}}
	res, err := Run(context.Background(), llm, nil, "p/m", Task{Name: "t", Prompt: "тема", Fields: fields})
	if err != nil || res.Attempts != 2 || res.Data["theme"] != "light" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	last := llm.calls[1][len(llm.calls[1])-1].Content
	if !strings.Contains(last, "нет JSON-объекта") {
		t.Fatalf("модели не объяснили ошибку: %q", last)
	}
	if !strings.Contains(llm.calls[0][0].Content, `"theme": string, обязательное, одно из: light, dark`) {
		t.Fatalf("системный промпт: %q", llm.calls[0][0].Content)
	}
}

func TestRunGivesUp(t *testing.T) {
	llm := &scripted{replies: []string{"a", "b", "c", "d"}}
	_, err := Run(context.Background(), llm, nil, "p/m", Task{Name: "t", Fields: fields})
	if err == nil || len(llm.calls) != Attempts {
		t.Fatalf("err=%v calls=%d", err, len(llm.calls))
	}
	if _, err := Run(context.Background(), llm, nil, "", Task{Fields: fields}); !errors.Is(err, ErrNoModel) {
		t.Fatalf("без модели: %v", err)
	}
	if _, err := Run(context.Background(), llm, nil, "p/m", Task{Fields: []Field{{Name: "x", Type: "date"}}}); err == nil {
		t.Fatal("неизвестный тип принят")
	}
}

func TestCheapest(t *testing.T) {
	ps := []store.Provider{
		{Name: "web", Kind: store.KindWeb, Models: []string{"chat"}},
		{Name: "big", Kind: store.KindAnthropic, Models: []string{"opus"}, PriceIn: 15, PriceOut: 75},
		{Name: "small", Models: []string{"mini", "max"}, PriceIn: 0.15, PriceOut: 0.6},
		{Name: "empty"},
	}
	if got := Cheapest(ps); got != "small/mini" {
		t.Fatalf("got %q", got)
	}
	if got := Cheapest(ps[:1]); got != "web/chat" {
		t.Fatalf("веб как запасной вариант: %q", got)
	}
	if Cheapest(nil) != "" {
		t.Fatal("без провайдеров должна быть пустая строка")
	}
}
