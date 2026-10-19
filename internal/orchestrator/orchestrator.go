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
	"slices"
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
	// Subtasks — задача разбита для нескольких агентов (пусто — один агент).
	Subtasks []Subtask `json:"subtasks,omitempty"`
	// Agent — постоянный агент, которому Оркестратор отдаёт задачу целиком.
	Agent string `json:"agent,omitempty"`
	// Models — исполнитель и резерв, которых Оркестратор выбрал сам (режим «Авто»).
	Models []string `json:"models,omitempty"`
	// Reviewer — ревьюер, выбранный Оркестратором: «провайдер/модель» или off.
	Reviewer string `json:"reviewer,omitempty"`
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
	// Refused — модель отказывалась работать исполнителем и ни разу не
	// справилась (например, CLI без прав на свои инструменты): не назначать.
	Refused bool
}

// Context — что Оркестратор знает кроме проекта: кто будет исполнять,
// какие ещё модели есть, какие скиллы и MCP-серверы можно выдать.
type Context struct {
	Executors []Executor // выбранные исполнитель и резерв; пусто — ещё не выбраны
	Others    []Executor // остальные модели — чтобы посоветовать замену
	Skills    []Skill
	Servers   []Server
	// Memory — выжимка памяти проекта: закреплённое и похожее на просьбу.
	Memory string
	// Git — проект в git: можно делить задачу на подзадачи для нескольких
	// агентов (каждый работает в своей копии проекта). Parallel — сколько
	// агентов работают одновременно.
	Git      bool
	Parallel int
	// Team — постоянные агенты проекта.
	Team []Member
	// Auto — исполнителя, резерв и ревьюера Оркестратор выбирает сам по
	// сложности задачи (тогда Executors пуст, а все модели — в Others).
	Auto bool
}

// Member — постоянный агент команды проекта.
type Member struct {
	Name  string
	Role  string
	Scope []string
	Model string
	Stats string // «сделал 5 задач, 1 не удалась»
}

// Subtask — часть большой задачи для отдельного агента.
type Subtask struct {
	Title string `json:"title"`
	Goal  string `json:"goal"`
	Model string `json:"model"`
	Agent string `json:"agent,omitempty"` // имя постоянного агента команды
	After []int  `json:"after,omitempty"`
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
	if e.Refused {
		line += "; НЕ ГОДИТСЯ исполнителем — отказывается работать в фоне, не назначай её ни исполнителем, ни резервом"
	}
	return line + "\n"
}

// instructions — раздел системного промпта про исполнителей, скиллы и MCP.
func instructions(c Context) string {
	var b strings.Builder
	mcpOK := false
	if strings.TrimSpace(c.Memory) != "" {
		b.WriteString("\n\nПамять проекта (закреплённое и похожее на эту задачу) — учитывай в плане:\n" + c.Memory)
	}
	b.WriteString("\n\nИсполнитель и его возможности.\n")
	if c.Auto {
		b.WriteString(autoRules)
		for _, e := range c.Others {
			mcpOK = mcpOK || e.MCP
		}
	} else if len(c.Executors) == 0 {
		b.WriteString("Исполнитель ещё не выбран: не выдавай MCP-серверы, скиллы выдавать можно.\n")
	} else {
		b.WriteString("Задачу выполнит (первый — основной, дальше — резерв Circuit Breaker):\n")
		for _, e := range c.Executors {
			b.WriteString(capsLine(e))
		}
		mcpOK = c.Executors[0].MCP
	}
	if len(c.Others) > 0 {
		if c.Auto {
			b.WriteString("Модели, из которых выбирать:\n")
		} else {
			b.WriteString("Другие доступные модели:\n")
		}
		for _, e := range c.Others {
			b.WriteString(capsLine(e))
		}
	}
	b.WriteString("Если задаче нужно то, чего исполнитель не умеет (картинки, звук, голос, интернет), скажи об этом и посоветуй модель, которая умеет.\n")
	b.WriteString("Если по статистике есть модель дешевле, которая обычно справляется с такими задачами, или выбранная часто проваливается, коротко посоветуй замену.\n")

	if len(c.Team) > 0 {
		b.WriteString("\nКоманда проекта — постоянные агенты. У каждого своя роль, зона и память о прошлых задачах в этой зоне, поэтому задачу из его зоны лучше отдать ему. Назначь агента строкой в плане:\nАгент: имя\nВ подзадачах — полем \"agent\":\"имя\" (модель агента подставится сама). Если задача ни в чьей зоне, агента не назначай.\n")
		for _, mb := range c.Team {
			line := "- " + mb.Name + " (" + mb.Model + ")"
			if mb.Role != "" {
				line += ": " + firstSentence(mb.Role)
			}
			if len(mb.Scope) > 0 {
				line += "; зона: " + strings.Join(mb.Scope, ", ")
			}
			if mb.Stats != "" {
				line += "; " + mb.Stats
			}
			b.WriteString(line + "\n")
		}
	}
	if len(c.Skills) > 0 {
		b.WriteString("\nСкиллы — инструкции проекта. Выдай исполнителю только те, что относятся к задаче, отдельной строкой в плане:\nСкиллы: имя1, имя2\nЕсли ни один не нужен, строку не пиши.\nДоступные скиллы:\n")
		for _, sk := range c.Skills {
			fmt.Fprintf(&b, "- %s — %s\n", sk.Name, sk.Description)
		}
	}

	if c.Git {
		par := c.Parallel
		if par <= 0 {
			par = 2
		}
		fmt.Fprintf(&b, `
Несколько агентов. Если задача большая и делится на независимые части (разные файлы или модули), раздели её
на 2–6 подзадач: их сделают разные агенты одновременно (до %d сразу), каждый в своей копии проекта, потом Edy
сольёт результат и прогонит общие проверки. Простую задачу или задачу, где все правки в одном месте, не дели.
Части не должны править одни и те же строки — иначе будет конфликт. Каждой подзадаче выбери исполнителя из
моделей выше по возможностям и статистике: простое — дешёвой модели, сложное — сильной. Если часть зависит от
другой, укажи её номер в after (только меньшие номера). Блок — после плана:
<subtasks>[{"title":"коротко","goal":"что сделать и в каких файлах","model":"провайдер/модель","after":[1]}]</subtasks>
`, par)
	} else {
		b.WriteString("\nПроект без git: задачу на подзадачи для нескольких агентов не дели.\n")
	}
	b.WriteString("\nMCP-серверы. ")
	if c.Auto && mcpOK {
		b.WriteString("Выдавай их, только если выбранный тобой исполнитель умеет MCP (см. «скиллы получает через MCP Edy»). ")
	}
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

// autoRules — как Оркестратору выбирать модели самому (режим «Авто»).
const autoRules = `Исполнителя выбираешь ты сам — пользователь доверил это тебе. Оцени сложность задачи и выбери из списка ниже:
- простая (опечатка, текст, мелкая правка в одном файле, конфиг) — быстрая и дешёвая модель (flash, mini, haiku, lite и т.п.);
- средняя (новая функция, правка в нескольких файлах, тесты) — модель среднего уровня;
- сложная (архитектура, много модулей, трудная отладка, безопасность, параллелизм) — самая сильная модель.
Учитывай возможности (картинки, MCP) и статистику: модель, которая часто проваливается на таких задачах, не бери.
Резерв (Circuit Breaker) — другая модель, не слабее исполнителя, лучше у другого провайдера.
Ревьюер — другая модель, чем исполнитель, сильная в чтении кода; для совсем простой задачи ревью не нужно — пиши «нет».
Если задачу отдаёшь агенту команды, его модель подставится сама, но строки всё равно напиши.
Выбор напиши в плане отдельными строками, имена моделей — точно как в списке:
Исполнитель: провайдер/модель
Резерв: провайдер/модель
Ревьюер: провайдер/модель или нет
И одной фразой в ответе объясни выбор (почему такая модель хватит или нужна).
`

var (
	executorLineRe = regexp.MustCompile(`(?mi)^[\s>*_-]*Исполнитель\s*\**\s*:\s*\**\s*(.+?)\s*$`)
	fallbackLineRe = regexp.MustCompile(`(?mi)^[\s>*_-]*Резерв\s*\**\s*:\s*\**\s*(.+?)\s*$`)
	reviewerLineRe = regexp.MustCompile(`(?mi)^[\s>*_-]*Ревьюер\s*\**\s*:\s*\**\s*(.+?)\s*$`)
)

// PlanModels достаёт из плана выбор Оркестратора: исполнителя с резервом и
// ревьюера. Берутся только известные модели; ревьюер «нет» — off.
func PlanModels(plan string, c Context) (models []string, reviewer string) {
	known := map[string]bool{}
	for _, e := range append(append([]Executor{}, c.Executors...), c.Others...) {
		known[e.Model] = !e.Refused
	}
	line := func(re *regexp.Regexp) string {
		m := re.FindStringSubmatch(plan)
		if m == nil {
			return ""
		}
		return strings.Trim(m[1], "`*_ .")
	}
	for _, m := range []string{line(executorLineRe), line(fallbackLineRe)} {
		if known[m] && !slices.Contains(models, m) {
			models = append(models, m)
		}
	}
	switch r := line(reviewerLineRe); {
	case known[r]:
		reviewer = r
	case strings.HasPrefix(strings.ToLower(r), "нет"), strings.HasPrefix(strings.ToLower(r), "не нуж"), strings.EqualFold(r, "off"):
		reviewer = "off"
	}
	return models, reviewer
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
	mcp := len(c.Executors) > 0 && c.Executors[0].MCP
	if c.Auto {
		out.Models, out.Reviewer = PlanModels(out.Plan, c)
		for _, e := range c.Others {
			mcp = mcp || len(out.Models) > 0 && e.Model == out.Models[0] && e.MCP
		}
	}
	if mcp {
		out.MCP = PlanMCP(out.Plan, c.Servers)
	}
	out.Skills = PlanSkills(out.Plan, c.Skills)
	out.Agent = PlanAgent(out.Plan, c.Team)
	if c.Git {
		out.Subtasks = checkSubtasks(out.Subtasks, c)
	} else {
		out.Subtasks = nil
	}
	return out, nil
}

// checkSubtasks оставляет только осмысленные подзадачи: модель из
// доступных (иначе — основной исполнитель), зависимости — на более ранние.
func checkSubtasks(list []Subtask, c Context) []Subtask {
	known := map[string]bool{}
	fallback := ""
	for _, e := range append(append([]Executor{}, c.Executors...), c.Others...) {
		known[e.Model] = !e.Refused
		if fallback == "" && !e.Refused {
			fallback = e.Model
		}
	}
	var out []Subtask
	for _, st := range list {
		if strings.TrimSpace(st.Goal) == "" || len(out) == 6 {
			continue
		}
		if st.Agent != "" {
			st.Agent = memberName(st.Agent, c.Team)
		}
		if !known[st.Model] {
			st.Model = fallback
		}
		var after []int
		for _, a := range st.After {
			if a >= 1 && a <= len(out) {
				after = append(after, a)
			}
		}
		st.After = after
		if strings.TrimSpace(st.Title) == "" {
			st.Title = st.Goal
		}
		out = append(out, st)
	}
	if len(out) < 2 {
		return nil
	}
	return out
}

var (
	mcpLineRe    = regexp.MustCompile(`(?mi)^\s*[-*]?\s*MCP\s*:\s*(.+)$`)
	agentLineRe  = regexp.MustCompile(`(?mi)^\s*[-*]?\s*(?:Агент|Agent)\s*:\s*(.+)$`)
	skillsLineRe = regexp.MustCompile(`(?mi)^\s*[-*]?\s*(?:Скиллы|Skills)\s*:\s*(.+)$`)
	newMCPRe     = regexp.MustCompile(`(?s)<new_mcp>\s*(.*?)\s*</new_mcp>`)
	subtasksRe   = regexp.MustCompile(`(?s)<subtasks>\s*(.*?)\s*</subtasks>`)
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
	if m := subtasksRe.FindStringSubmatch(content); m != nil {
		json.Unmarshal([]byte(m[1]), &out.Subtasks)
		content = subtasksRe.ReplaceAllString(content, "")
	}
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

// PlanAgent достаёт из плана строку «Агент: имя» — только агент команды.
func PlanAgent(plan string, team []Member) string {
	m := agentLineRe.FindStringSubmatch(plan)
	if m == nil {
		return ""
	}
	return memberName(m[1], team)
}

// memberName находит агента команды по имени (без учёта регистра и кавычек).
func memberName(name string, team []Member) string {
	name = strings.TrimSpace(strings.Trim(strings.TrimSpace(name), "`*.\"'«»"))
	for _, mb := range team {
		if strings.EqualFold(mb.Name, name) {
			return mb.Name
		}
	}
	return ""
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, ".\n"); i > 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > 140 {
		s = string(r[:140]) + "…"
	}
	return s
}
