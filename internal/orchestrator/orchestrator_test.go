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
	r, err := Chat(context.Background(), llm, "agy/claude-sonnet-4-6", dir, []agent.Msg{{Role: "user", Content: "Добавь вычитание"}}, Context{})
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
	if !strings.Contains(llm.got[0].Content, "Исполнитель ещё не выбран: не выдавай MCP-серверы") || r.MCP != nil {
		t.Fatal("без исполнителя MCP не выдаются")
	}

	llm.reply = "Уточните: нужен ли тест?"
	if r, _ := Chat(context.Background(), llm, "m", dir, nil, Context{}); r.Plan != "" || r.Text != "Уточните: нужен ли тест?" {
		t.Fatalf("без плана: %+v", r)
	}
	if _, err := Chat(context.Background(), llm, "", dir, nil, Context{}); err == nil {
		t.Fatal("без модели должна быть ошибка")
	}
	if g := Goal("Добавь Sub", "1. шаг"); g != "Добавь Sub\n\nУтверждённый план:\n1. шаг" {
		t.Fatalf("goal = %q", g)
	}
}

func TestOrchestratorGivesMCPAndSkills(t *testing.T) {
	dir := t.TempDir()
	c := Context{
		Executors: []Executor{{Model: "cc/sonnet", Caps: []string{"MCP", "Картинки"}, MCP: true}},
		Others:    []Executor{{Model: "agy/gemini", Caps: []string{"Интернет"}}},
		Skills:    []Skill{{Name: "go-tests", Description: "как писать тесты"}, {Name: "migrations", Description: "миграции БД"}},
		Servers:   []Server{{Name: "playwright", Description: "браузер"}, {Name: "github", Description: "issues и PR", Account: "работа"}},
	}
	llm := &fakeLLM{reply: "Нужен ещё сервер для Figma.\n<new_mcp>{\"name\":\"figma\",\"description\":\"макеты\",\"type\":\"http\",\"url\":\"https://figma.example/mcp\",\"secrets\":\"токен Figma\"}</new_mcp>\n<new_mcp>{\"name\":\"bad\"}</new_mcp>\n<plan>\nЗадача: починить форму входа.\n1. login.go.\nСкиллы: go-tests, выдуманный\nMCP: playwright, Выдуманный, PLAYWRIGHT\n</plan>"}
	r, err := Chat(context.Background(), llm, "m", dir, []agent.Msg{{Role: "user", Content: "Почини вход"}}, c)
	if err != nil {
		t.Fatal(err)
	}
	sys := llm.got[0].Content
	for _, want := range []string{
		"- cc/sonnet: MCP, Картинки; скиллы получает через MCP Edy",
		"- agy/gemini: Интернет; скиллы получает в промпте",
		"- go-tests — как писать тесты", "Скиллы: имя1, имя2",
		"- github — issues и PR (аккаунт: работа)", "MCP: имя1, имя2", "<new_mcp>",
	} {
		if !strings.Contains(sys, want) {
			t.Fatalf("в инструкциях нет %q:\n%s", want, sys)
		}
	}
	if len(r.MCP) != 1 || r.MCP[0] != "playwright" || len(r.Skills) != 1 || r.Skills[0] != "go-tests" {
		t.Fatalf("выдано: mcp=%v skills=%v", r.MCP, r.Skills)
	}
	if len(r.NewMCP) != 1 || r.NewMCP[0].Name != "figma" || strings.Contains(r.Text, "new_mcp") || r.Text != "Нужен ещё сервер для Figma." {
		t.Fatalf("предложение нового MCP: %+v / %q", r.NewMCP, r.Text)
	}

	// Исполнитель без MCP: серверы не выдаются, даже если они в плане.
	c.Executors = []Executor{{Model: "agy/gemini"}}
	r, _ = Chat(context.Background(), llm, "m", dir, nil, c)
	if r.MCP != nil || !strings.Contains(llm.got[0].Content, "Основной исполнитель не умеет MCP") || strings.Contains(llm.got[0].Content, "- playwright") {
		t.Fatalf("без MCP: %v\n%s", r.MCP, llm.got[0].Content)
	}
	if len(r.Skills) != 1 {
		t.Fatalf("скиллы выдаются и без MCP (в промпте): %v", r.Skills)
	}
}

func TestPlanParsers(t *testing.T) {
	servers := []Server{{Name: "playwright"}, {Name: "github"}}
	if got := PlanMCP("1. шаг\n- MCP: `github`; playwright", servers); len(got) != 2 || got[0] != "github" {
		t.Fatalf("PlanMCP: %v", got)
	}
	if got := PlanMCP("1. шаг", servers); got != nil {
		t.Fatalf("без строки MCP: %v", got)
	}
	if got := PlanSkills("Skills: «go-tests»", []Skill{{Name: "go-tests"}}); len(got) != 1 {
		t.Fatalf("PlanSkills: %v", got)
	}
}
