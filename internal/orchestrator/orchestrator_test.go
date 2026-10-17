package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laurenwilcox/name/internal/agent"
)

type fakeLLM struct {
	got   []agent.Msg
	tools []agent.ToolDef
	reply string
}

func (f *fakeLLM) Chat(ctx context.Context, model string, msgs []agent.Msg, tools []agent.ToolDef) (agent.Msg, error) {
	f.got, f.tools = msgs, tools
	return agent.Msg{Role: "assistant", Content: f.reply}, nil
}

func TestChatSeesProjectAndExtractsPlan(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "calc.go"), []byte("package calc"), 0o644)
	llm := &fakeLLM{reply: "Предлагаю так.\n<plan>\nЗадача: добавить Sub.\n1. calc.go — функция Sub.\nПроверка: go test.\n</plan>"}
	r, err := Chat(context.Background(), llm, "agy/claude-sonnet-4-6", dir, []agent.Msg{{Role: "user", Content: "Добавь вычитание"}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Text != "Предлагаю так." || !strings.HasPrefix(r.Plan, "Задача: добавить Sub.") {
		t.Fatalf("r = %+v", r)
	}
	if !strings.Contains(llm.got[0].Content, "calc.go") || llm.got[1].Content != "Добавь вычитание" {
		t.Fatalf("Оркестратор должен видеть файлы проекта и историю: %+v", llm.got)
	}
	if llm.tools != nil {
		t.Fatal("Оркестратору инструменты не передаются")
	}

	llm.reply = "Уточните: нужен ли тест?"
	if r, _ := Chat(context.Background(), llm, "m", dir, nil); r.Plan != "" || r.Text != "Уточните: нужен ли тест?" {
		t.Fatalf("без плана: %+v", r)
	}
	if _, err := Chat(context.Background(), llm, "", dir, nil); err == nil {
		t.Fatal("без модели должна быть ошибка")
	}
	if g := Goal("Добавь Sub", "1. шаг"); g != "Добавь Sub\n\nУтверждённый план:\n1. шаг" {
		t.Fatalf("goal = %q", g)
	}
}
