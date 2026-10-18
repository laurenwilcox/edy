// Package orchestrator — Оркестратор Edy: обсуждает задачу с разработчиком,
// смотрит на структуру проекта и составляет план для агента-исполнителя.
// Код он не пишет.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/laurenwilcox/name/internal/agent"
	"github.com/laurenwilcox/name/internal/codeindex"
)

const systemPrompt = `Ты — Оркестратор Edy. Ты не пишешь код сам: ты обсуждаешь задачу с разработчиком и готовишь план для агента-исполнителя.

Как работать:
- Если задача понятна не до конца, задай 1–3 коротких уточняющих вопроса.
- Когда всё ясно, составь короткий план и выведи его блоком:
<plan>
Задача: одно предложение, что нужно сделать.
1. Первый шаг (какие файлы и что в них поменять).
2. …
Проверка: как убедиться, что всё работает (какие тесты добавить или запустить).
</plan>
- Вне блока объясни план в 1–2 предложениях. Разработчик утвердит его кнопкой «Начать работу» или попросит поправить.
- Опирайся на реальную структуру проекта ниже, не выдумывай файлы.
Отвечай по-русски, кратко.

Проект: %s
Скелет проекта (файлы, типы и функции без тел):
%s`

var planRe = regexp.MustCompile(`(?s)<plan>\s*(.*?)\s*</plan>`)

// Reply — ответ Оркестратора.
type Reply struct {
	Text string   `json:"text"`           // ответ без блока плана
	Plan string   `json:"plan,omitempty"` // план, если Оркестратор его составил
	MCP  []string `json:"mcp,omitempty"`  // внешние MCP-серверы, которые он выдаёт исполнителю
}

// Server — внешний MCP-сервер, который Оркестратор может выдать исполнителю.
type Server struct {
	Name        string
	Description string
}

const mcpPrompt = `

Внешние MCP-серверы. Исполнитель всегда получает MCP Edy (файлы проекта, поиск, проверки, скиллы).
Дополнительно ты можешь выдать ему серверы из списка ниже — только те, без которых задачу не сделать
или не проверить. Для этого добавь в план отдельную строку:
MCP: имя1, имя2
Если дополнительные серверы не нужны, строку не пиши. Серверы получает только исполнитель с поддержкой MCP
(Claude Code); остальным они не передаются.
Доступные серверы:
%s`

// Chat отправляет историю диалога Оркестратору и возвращает его ответ.
// servers — каталог внешних MCP-серверов, из которых он выбирает нужные.
func Chat(ctx context.Context, llm agent.LLM, model, project string, history []agent.Msg, servers []Server) (Reply, error) {
	if model == "" {
		return Reply{}, errors.New("выберите модель для Оркестратора")
	}
	idx, err := codeindex.Build(ctx, project)
	if err != nil {
		return Reply{}, fmt.Errorf("не удалось прочитать проект: %w", err)
	}
	files := idx.Skeleton(15000)
	system := fmt.Sprintf(systemPrompt, project, files)
	if len(servers) > 0 {
		var list strings.Builder
		for _, sv := range servers {
			fmt.Fprintf(&list, "- %s — %s\n", sv.Name, sv.Description)
		}
		system += fmt.Sprintf(mcpPrompt, list.String())
	}
	msgs := append([]agent.Msg{{Role: "system", Content: system}}, history...)
	// Оркестратору инструменты не нужны: он только планирует.
	reply, err := llm.Chat(ctx, model, msgs, nil)
	if err != nil {
		return Reply{}, err
	}
	out := parse(reply.Content)
	out.MCP = PlanMCP(out.Plan, servers)
	return out, nil
}

var mcpLineRe = regexp.MustCompile(`(?mi)^\s*[-*]?\s*MCP\s*:\s*(.+)$`)

// PlanMCP достаёт из плана строку «MCP: …» и оставляет только серверы из
// каталога: выдумать сервер или выдать выключенный Оркестратор не может.
func PlanMCP(plan string, servers []Server) []string {
	known := map[string]bool{}
	for _, sv := range servers {
		known[sv.Name] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, m := range mcpLineRe.FindAllStringSubmatch(plan, -1) {
		for _, name := range strings.FieldsFunc(m[1], func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
			name = strings.ToLower(strings.Trim(name, "`*.\"'"))
			if known[name] && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out
}

func parse(content string) Reply {
	m := planRe.FindStringSubmatch(content)
	if m == nil {
		return Reply{Text: strings.TrimSpace(content)}
	}
	text := strings.TrimSpace(planRe.ReplaceAllString(content, ""))
	return Reply{Text: text, Plan: strings.TrimSpace(m[1])}
}

// Goal собирает задачу для агента из исходной просьбы и утверждённого плана.
func Goal(request, plan string) string {
	if strings.TrimSpace(request) == "" {
		return plan
	}
	return strings.TrimSpace(request) + "\n\nУтверждённый план:\n" + plan
}
