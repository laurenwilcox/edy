// Package review — ревьюер Edy: второй агент читает готовые изменения и ищет
// то, что тесты не ловят, — до того, как вы нажмёте Approve.
package review

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/laurenwilcox/name/internal/agent"
	"github.com/laurenwilcox/name/internal/store"
	"github.com/laurenwilcox/name/internal/tasks"
)

const prompt = `Ты — ревьюер кода в Edy. Тебе дают задачу, утверждённый план и изменения (diff).
Линтеры и тесты уже прошли. Ищи то, что они не ловят:
- ошибки логики и неучтённые крайние случаи;
- расхождение с задачей и планом, лишние или незаконченные изменения;
- проблемы безопасности (секреты в коде, инъекции, небезопасные права);
- новую логику без тестов.
К стилю и вкусовщине не придирайся.

Ответь строго в формате. Первая строка:
ВЕРДИКТ: OK
или
ВЕРДИКТ: ЗАМЕЧАНИЯ
Если есть замечания — дальше нумерованный список, не больше 7 пунктов: файл — проблема — что сделать.
Отвечай по-русски, кратко.`

var verdictRe = regexp.MustCompile(`(?i)ВЕРДИКТ\s*:\s*\**\s*(OK|ОК|ЗАМЕЧАНИЯ|ISSUES)`)

// Pick выбирает модель ревьюера для задачи; пусто — ревью не нужно.
type Pick func(v tasks.View) string

// New возвращает ревьюера для tasks.Manager.
func New(llm agent.LLM, journal tasks.Journal, pick Pick) tasks.Reviewer {
	return func(ctx context.Context, v tasks.View, patch string) (tasks.Review, error) {
		model := pick(v)
		if model == "" {
			return tasks.Review{}, nil
		}
		if strings.TrimSpace(patch) == "" {
			return tasks.Review{Model: model, Verdict: "ok", Text: "Изменений нет — проверять нечего."}, nil
		}
		id := v.ID + "-review"
		if journal != nil {
			journal.CreateAgent(store.Agent{ID: id, Role: store.RoleReviewer, Model: model, Project: v.Project, TaskID: v.ID, Goal: "Ревью: " + v.Title, Created: time.Now()})
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Задача:\n%s\n\n", v.Goal)
		if v.Summary != "" {
			fmt.Fprintf(&b, "Итог исполнителя (%s): %s\n\n", v.Model, v.Summary)
		}
		fmt.Fprintf(&b, "Изменения:\n```diff\n%s\n```", patch)
		reply, err := llm.Chat(ctx, model, []agent.Msg{{Role: "system", Content: prompt}, {Role: "user", Content: b.String()}}, nil)
		if err != nil {
			if journal != nil {
				journal.FinishAgent(id, store.AgentFailed, "", err.Error())
			}
			return tasks.Review{Model: model}, err
		}
		rv := Parse(reply.Content)
		rv.Model = model
		if journal != nil {
			journal.AddAgentEvent(id, "review", reply.Content, time.Now())
			summary := "Замечаний нет"
			if rv.Verdict == "issues" {
				summary = "Есть замечания"
			}
			journal.FinishAgent(id, store.AgentDone, summary, "")
		}
		return rv, nil
	}
}

// Parse разбирает ответ ревьюера. Без явного вердикта ответ считается
// замечаниями: автопринятие не должно пройти по непонятному ответу.
func Parse(text string) tasks.Review {
	text = strings.TrimSpace(text)
	m := verdictRe.FindStringSubmatchIndex(text)
	if m == nil {
		if text == "" {
			return tasks.Review{Verdict: "error", Text: "ревьюер вернул пустой ответ"}
		}
		return tasks.Review{Verdict: "issues", Text: text}
	}
	verdict := strings.ToUpper(text[m[2]:m[3]])
	rest := strings.TrimSpace(strings.Trim(strings.TrimSpace(text[m[1]:]), "*"))
	if verdict == "OK" || verdict == "ОК" {
		return tasks.Review{Verdict: "ok", Text: rest}
	}
	return tasks.Review{Verdict: "issues", Text: rest}
}

// AutoModel — ревьюер по умолчанию: другая модель из цепочки задачи (обычно
// резервная), иначе та же, что исполняла.
func AutoModel(v tasks.View) string {
	for _, m := range v.Models {
		if m != v.Model {
			return m
		}
	}
	return v.Model
}
