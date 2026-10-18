// Package orchestrator — Оркестратор Edy: обсуждает задачу с разработчиком,
// смотрит на структуру проекта и составляет план для агента-исполнителя.
// Код он не пишет.
package orchestrator

import (
	"context"
	"encoding/json"
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
	Text   string     `json:"text"`             // ответ без служебных блоков
	Plan   string     `json:"plan,omitempty"`   // план, если Оркестратор его составил
	MCP    []string   `json:"mcp,omitempty"`    // внешние MCP-серверы для исполнителя
	Skills []string   `json:"skills,omitempty"` // скиллы для исполнителя
	NewMCP []Proposal `json:"new_mcp,omitempty"`
}

// Server — внешний MCP-сервер из каталога.
type Server struct {
	Name        string
	Description string
	Account     string
}

// Skill — скилл, который можно выдать исполнителю.
type Skill struct {
	Name        string
	Description string
}

// Executor — модель и её возможности (заголовки тегов).
type Executor struct {
	Model string
	Caps  []string
	MCP   bool // умеет MCP: получает MCP-серверы, а скиллы — через MCP Edy
	// Stats — как модель справлялась с задачами и сколько стоила (за 30 дней).
	Stats string
}

// Context — что Оркестратор знает кроме проекта: кто будет исполнять,
// какие ещё модели есть, какие скиллы и MCP-серверы можно выдать.
type Context struct {
	Executors []Executor // выбранные исполнитель и резерв; пусто — ещё не выбраны
	Others    []Executor // остальные модели — чтобы посоветовать замену
	Skills    []Skill
	Servers   []Server
}

// Proposal — новый MCP-сервер, которого нет в каталоге. Его добавляет
// пользователь: Оркестратор только предлагает.
type Proposal struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Type        string   `json:"type"`
	URL         string   `json:"url,omitempty"`
	Command     string   `json:"command,omitempty"`
	Args        []string `json:"args,omitempty"`
	Secrets     string   `json:"secrets,omitempty"` // какие ключи понадобятся
}

func capsLine(e Executor) string {
	caps := "без особых возможностей"
	if len(e.Caps) > 0 {
		caps = strings.Join(e.Caps, ", ")
	}
	how := "скиллы получает в промпте"
	if e.MCP {
		how = "скиллы получает через MCP Edy"
	}
	line := fmt.Sprintf("- %s: %s; %s", e.Model, caps, how)
	if e.Stats != "" {
		line += "; " + e.Stats
	}
	return line + "\n"
}

// instructions — раздел системного промпта про исполнителей, скиллы и MCP.
func instructions(c Context) string {
	var b strings.Builder
	mcpOK := false
	b.WriteString("\n\nИсполнитель и его возможности.\n")
	if len(c.Executors) == 0 {
		b.WriteString("Исполнитель ещё не выбран: не выдавай MCP-серверы, скиллы выдавать можно.\n")
	} else {
		b.WriteString("Задачу выполнит (первый — основной, дальше — резерв Circuit Breaker):\n")
		for _, e := range c.Executors {
			b.WriteString(capsLine(e))
		}
		mcpOK = c.Executors[0].MCP
	}
	if len(c.Others) > 0 {
		b.WriteString("Другие доступные модели:\n")
		for _, e := range c.Others {
			b.WriteString(capsLine(e))
		}
	}
	b.WriteString("Если задаче нужно то, чего исполнитель не умеет (картинки, звук, голос, интернет), скажи об этом и посоветуй модель, которая умеет.\n")
	b.WriteString("Если по статистике есть модель дешевле, которая обычно справляется с такими задачами, или выбранная часто проваливается, коротко посоветуй замену.\n")

	if len(c.Skills) > 0 {
		b.WriteString("\nСкиллы — инструкции проекта. Выдай исполнителю только те, что относятся к задаче, отдельной строкой в плане:\nСкиллы: имя1, имя2\nЕсли ни один не нужен, строку не пиши.\nДоступные скиллы:\n")
		for _, sk := range c.Skills {
			fmt.Fprintf(&b, "- %s — %s\n", sk.Name, sk.Description)
		}
	}

	b.WriteString("\nMCP-серверы. ")
	if !mcpOK {
		b.WriteString("Основной исполнитель не умеет MCP: строку MCP не пиши.\n")
		return b.String()
	}
	b.WriteString("Исполнитель всегда получает MCP Edy (файлы проекта, поиск, проверки, скиллы). Дополнительные серверы выдавай, только если без них задачу не сделать или не проверить, отдельной строкой в плане:\nMCP: имя1, имя2\nЕсли не нужны, строку не пиши.\n")
	if len(c.Servers) > 0 {
		b.WriteString("Сначала бери серверы из каталога Edy — они уже настроены, с ключами пользователя:\n")
		for _, sv := range c.Servers {
			line := "- " + sv.Name + " — " + sv.Description
			if sv.Account != "" {
				line += " (аккаунт: " + sv.Account + ")"
			}
			b.WriteString(line + "\n")
		}
	} else {
		b.WriteString("Каталог MCP-серверов пуст.\n")
	}
	b.WriteString(`Если в каталоге нет нужного сервера, а ты точно знаешь известный MCP-сервер для этого, предложи его вне плана блоком:
<new_mcp>{"name":"имя","description":"для чего","type":"stdio","command":"npx","args":["пакет"],"secrets":"какие ключи нужны"}</new_mcp>
(для удалённого сервера: "type":"http","url":"https://…"). Пользователь сам добавит его в каталог, после этого его можно выдавать. Не выдумывай пакеты и адреса.
`)
	return b.String()
}

// Chat отправляет историю диалога Оркестратору и возвращает его ответ.
func Chat(ctx context.Context, llm agent.LLM, model, project string, history []agent.Msg, c Context) (Reply, error) {
	if model == "" {
		return Reply{}, errors.New("выберите модель для Оркестратора")
	}
	idx, err := codeindex.Build(ctx, project)
	if err != nil {
		return Reply{}, fmt.Errorf("не удалось прочитать проект: %w", err)
	}
	files := idx.Skeleton(15000)
	system := fmt.Sprintf(systemPrompt, project, files) + instructions(c)
	msgs := append([]agent.Msg{{Role: "system", Content: system}}, history...)
	// Оркестратору инструменты не нужны: он только планирует.
	reply, err := llm.Chat(ctx, model, msgs, nil)
	if err != nil {
		return Reply{}, err
	}
	out := parse(reply.Content)
	if len(c.Executors) > 0 && c.Executors[0].MCP {
		out.MCP = PlanMCP(out.Plan, c.Servers)
	}
	out.Skills = PlanSkills(out.Plan, c.Skills)
	return out, nil
}

var (
	mcpLineRe    = regexp.MustCompile(`(?mi)^\s*[-*]?\s*MCP\s*:\s*(.+)$`)
	skillsLineRe = regexp.MustCompile(`(?mi)^\s*[-*]?\s*(?:Скиллы|Skills)\s*:\s*(.+)$`)
	newMCPRe     = regexp.MustCompile(`(?s)<new_mcp>\s*(.*?)\s*</new_mcp>`)
)

// pick достаёт имена из строк плана и оставляет только известные.
func pick(re *regexp.Regexp, plan string, known map[string]bool) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(plan, -1) {
		for _, name := range strings.FieldsFunc(m[1], func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
			name = strings.Trim(name, "`*.\"'«»")
			for k := range known {
				if strings.EqualFold(k, name) {
					name = k
				}
			}
			if known[name] && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out
}

// PlanMCP достаёт из плана строку «MCP: …» и оставляет только серверы из
// каталога: выдумать сервер или выдать выключенный Оркестратор не может.
func PlanMCP(plan string, servers []Server) []string {
	known := map[string]bool{}
	for _, sv := range servers {
		known[sv.Name] = true
	}
	return pick(mcpLineRe, plan, known)
}

// PlanSkills достаёт из плана строку «Скиллы: …» — только существующие скиллы.
func PlanSkills(plan string, list []Skill) []string {
	known := map[string]bool{}
	for _, sk := range list {
		known[sk.Name] = true
	}
	return pick(skillsLineRe, plan, known)
}

func parse(content string) Reply {
	var out Reply
	for _, m := range newMCPRe.FindAllStringSubmatch(content, -1) {
		var p Proposal
		if json.Unmarshal([]byte(m[1]), &p) == nil && p.Name != "" && (p.Type == "stdio" && p.Command != "" || p.Type == "http" && p.URL != "") {
			out.NewMCP = append(out.NewMCP, p)
		}
	}
	content = newMCPRe.ReplaceAllString(content, "")
	m := planRe.FindStringSubmatch(content)
	if m == nil {
		out.Text = strings.TrimSpace(content)
		return out
	}
	out.Text = strings.TrimSpace(planRe.ReplaceAllString(content, ""))
	out.Plan = strings.TrimSpace(m[1])
	return out
}

// Goal собирает задачу для агента из исходной просьбы и утверждённого плана.
func Goal(request, plan string) string {
	if strings.TrimSpace(request) == "" {
		return plan
	}
	return strings.TrimSpace(request) + "\n\nУтверждённый план:\n" + plan
}
