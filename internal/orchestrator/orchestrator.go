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
	Text string `json:"text"`           // ответ без блока плана
	Plan string `json:"plan,omitempty"` // план, если Оркестратор его составил
}

// Chat отправляет историю диалога Оркестратору и возвращает его ответ.
func Chat(ctx context.Context, llm agent.LLM, model, project string, history []agent.Msg) (Reply, error) {
	if model == "" {
		return Reply{}, errors.New("выберите модель для Оркестратора")
	}
	idx, err := codeindex.Build(ctx, project)
	if err != nil {
		return Reply{}, fmt.Errorf("не удалось прочитать проект: %w", err)
	}
	files := idx.Skeleton(15000)
	msgs := append([]agent.Msg{{Role: "system", Content: fmt.Sprintf(systemPrompt, project, files)}}, history...)
	// Оркестратору инструменты не нужны: он только планирует.
	reply, err := llm.Chat(ctx, model, msgs, nil)
	if err != nil {
		return Reply{}, err
	}
	return parse(reply.Content), nil
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
