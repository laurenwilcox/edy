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
	os.WriteFile(filepath.Join(dir, "calc.go"), []byte("package calc\n\nfunc Add(a, b int) int { return a + b }\n"), 0o644)
	llm := &fakeLLM{reply: "Предлагаю так.\n<plan>\nЗадача: добавить Sub.\n1. calc.go — функция Sub.\nПроверка: go test.\n</plan>"}
	r, err := Chat(context.Background(), llm, "agy/claude-sonnet-4-6", dir, []agent.Msg{{Role: "user", Content: "Добавь вычитание"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Text != "Предлагаю так." || !strings.HasPrefix(r.Plan, "Задача: добавить Sub.") {
		t.Fatalf("r = %+v", r)
	}
	if !strings.Contains(llm.got[0].Content, "func Add(a, b int) int") || llm.got[1].Content != "Добавь вычитание" {
		t.Fatalf("Оркестратор должен видеть файлы проекта и историю: %+v", llm.got)
	}
	if llm.tools != nil {
		t.Fatal("Оркестратору инструменты не передаются")
	}
	if strings.Contains(llm.got[0].Content, "Внешние MCP-серверы") || r.MCP != nil {
		t.Fatal("без каталога о внешних MCP не говорим")
	}

	llm.reply = "Уточните: нужен ли тест?"
	if r, _ := Chat(context.Background(), llm, "m", dir, nil, nil); r.Plan != "" || r.Text != "Уточните: нужен ли тест?" {
		t.Fatalf("без плана: %+v", r)
	}
	if _, err := Chat(context.Background(), llm, "", dir, nil, nil); err == nil {
		t.Fatal("без модели должна быть ошибка")
	}
	if g := Goal("Добавь Sub", "1. шаг"); g != "Добавь Sub\n\nУтверждённый план:\n1. шаг" {
		t.Fatalf("goal = %q", g)
	}
}

func TestOrchestratorGivesMCPServers(t *testing.T) {
	dir := t.TempDir()
	servers := []Server{{Name: "playwright", Description: "браузер: открыть страницу, кликнуть, скриншот"}, {Name: "github", Description: "issues и PR"}}
	llm := &fakeLLM{reply: "<plan>\nЗадача: починить форму входа.\n1. login.go — проверка пароля.\nПроверка: открыть страницу в браузере.\nMCP: playwright, выдуманный, Playwright\n</plan>"}
	r, err := Chat(context.Background(), llm, "m", dir, []agent.Msg{{Role: "user", Content: "Почини вход"}}, servers)
	if err != nil {
		t.Fatal(err)
	}
	sys := llm.got[0].Content
	if !strings.Contains(sys, "- playwright — браузер: открыть страницу") || !strings.Contains(sys, "MCP: имя1, имя2") {
		t.Fatalf("инструкции Оркестратору:\n%s", sys)
	}
	if len(r.MCP) != 1 || r.MCP[0] != "playwright" {
		t.Fatalf("выданы MCP: %v", r.MCP)
	}
	if got := PlanMCP("1. шаг\n- MCP: `github`; playwright", servers); len(got) != 2 || got[0] != "github" {
		t.Fatalf("PlanMCP: %v", got)
	}
	if got := PlanMCP("1. шаг", servers); got != nil {
		t.Fatalf("без строки MCP: %v", got)
	}
}
