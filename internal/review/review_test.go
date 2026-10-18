package review

import (
	"context"
	"strings"
	"testing"

	"github.com/laurenwilcox/name/internal/agent"
	"github.com/laurenwilcox/name/internal/tasks"
)

func TestParse(t *testing.T) {
	for text, want := range map[string]string{
		"ВЕРДИКТ: OK":                                   "ok",
		"**ВЕРДИКТ: ОК**\nВсё хорошо":                   "ok",
		"ВЕРДИКТ: ЗАМЕЧАНИЯ\n1. calc.go — деление на 0": "issues",
		"Тут есть проблема с нулём":                     "issues",
		"": "error",
	} {
		if got := Parse(text).Verdict; got != want {
			t.Errorf("%q: %s, ждали %s", text, got, want)
		}
	}
	if r := Parse("ВЕРДИКТ: ЗАМЕЧАНИЯ\n1. a.go — нет теста"); r.Text != "1. a.go — нет теста" {
		t.Fatalf("текст: %q", r.Text)
	}
}

type fakeLLM struct {
	model, got string
}

func (f *fakeLLM) Chat(ctx context.Context, model string, msgs []agent.Msg, tools []agent.ToolDef) (agent.Msg, error) {
	f.model, f.got = model, msgs[1].Content
	return agent.Msg{Content: "ВЕРДИКТ: ЗАМЕЧАНИЯ\n1. calc.go — Div не проверяет ноль"}, nil
}

func TestReviewer(t *testing.T) {
	llm := &fakeLLM{}
	rv := New(llm, nil, AutoModel)
	v := tasks.View{ID: "t", Goal: "Добавь Div", Models: []string{"cheap/m", "smart/m"}, Model: "cheap/m", Summary: "добавил"}
	r, err := rv(context.Background(), v, "+func Div(a, b int) int { return a / b }")
	if err != nil || r.Verdict != "issues" || r.Model != "smart/m" || llm.model != "smart/m" {
		t.Fatalf("ревью: %+v %v (модель %s)", r, err, llm.model)
	}
	if !strings.Contains(llm.got, "Добавь Div") || !strings.Contains(llm.got, "return a / b") {
		t.Fatalf("ревьюер должен видеть задачу и diff:\n%s", llm.got)
	}
	if r, _ := rv(context.Background(), v, "  "); r.Verdict != "ok" {
		t.Fatal("пустой diff — замечаний нет")
	}
	off := New(llm, nil, func(tasks.View) string { return "" })
	if r, err := off(context.Background(), v, "x"); err != nil || r.Verdict != "" {
		t.Fatal("выключенное ревью")
	}
	if AutoModel(tasks.View{Models: []string{"a/m"}, Model: "a/m"}) != "a/m" {
		t.Fatal("одна модель — она же ревьюер")
	}
}
