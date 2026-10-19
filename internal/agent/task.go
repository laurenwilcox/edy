package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/laurenwilcox/name/internal/skills"
)

// Итог задачи.
const (
	StatusDone       = "done"        // хуки прошли, ждём Approve
	StatusNeedsHuman = "needs_human" // все модели исчерпали попытки, проект откачен
	StatusFailed     = "failed"      // ошибка самого Edy (git, отмена и т.п.)
)

// Event — запись в ленте задачи для интерфейса и терминала.
type Event struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"` // model, tool, checkpoint, hooks, breaker, info, error
	Text string    `json:"text"`
}

// Task — одна задача агенту в папке проекта.
type Task struct {
	Root string
	Goal string
	// Models — цепочка моделей «провайдер/модель»: если слабая трижды провалит
	// проверки, задачу получает следующая (Circuit Breaker).
	Models      []string
	LLM         LLM
	MaxAttempts int // провалов хуков до передачи задачи; по умолчанию 3
	MaxSteps    int // шагов модели на одну попытку; по умолчанию 40
	OnEvent     func(Event)
	// OnAttempt вызывается, когда задачу берёт очередная модель (новый агент);
	// OnAttemptEnd — когда она закончила: ok=true — проверки прошли.
	OnAttempt    func(model string)
	OnAttemptEnd func(model string, ok bool, note string)
	// Skills — скиллы проекта и общие; агент видит их список и грузит через use_skill.
	Skills []skills.Skill
	// MCP подключает MCP-сервер Edy к исполнителям, которые это умеют
	// (Claude Code): они правят проект инструментами Edy сами, без пошаговой
	// эмуляции. nil — все модели работают через инструменты в промпте.
	MCP MCPBridge
	// Role — инструкции постоянного агента (роль, зона), если задачу ведёт он.
	Role string
	// Memory — память проекта; MemoryBrief — закреплённые заметки, которые
	// исполнитель видит сразу (остальное ищет сам через project_memory).
	Memory      Memory
	MemoryBrief string
	// Code — поиск кода по смыслу для search_code; nil — только по словам.
	Code CodeSearch
	// Images — скриншоты к задаче (data:-URL); их получает модель, для
	// которой CanSee вернёт true, остальные — предупреждение в ленте.
	Images []string
	CanSee func(model string) bool
	// Extra — внешние MCP-серверы из каталога Edy, которые Оркестратор выдал
	// исполнителю вместе с MCP Edy (только исполнителям с поддержкой MCP).
	Extra []string
	// MaxWait — сколько модель может ждать сброса минутного лимита (429 с
	// коротким Retry-After) подряд, без единого ответа, прежде чем задача уйдёт
	// дальше; по умолчанию 5 минут. Каждый ответ модели сбрасывает счёт: медленная,
	// но работающая модель (бесплатный тариф Groq) доводит задачу до конца.
	MaxWait time.Duration

	// refused — почему последняя модель отказалась работать (для журнала).
	refused string
}

// maxRetryAfter — дольше этого лимит не ждём: значит, квота кончилась надолго.
const maxRetryAfter = 2 * time.Minute

// MCPBridge выдаёт исполнителю адрес MCP-сервера Edy, привязанный к задаче.
type MCPBridge interface {
	// Supports сообщает, умеет ли модель «провайдер/модель» подключать MCP.
	Supports(model string) bool
	// Bind возвращает адрес /mcp для проекта root: правки через него не
	// блокируются задачей, а вызовы и снимки уходят в ev. release — когда
	// исполнитель закончил.
	Bind(root string, ev MCPEvents) (url string, release func())
}

// MCPEvents — что делает исполнитель через MCP, и что ему видно.
type MCPEvents struct {
	// Skills — скиллы, доступные через use_skill (выданные Оркестратором).
	Skills []string
	// Memory — память проекта для project_memory и remember.
	Memory   Memory
	Code     CodeSearch
	OnCall   func(tool, note string, isErr bool)
	OnChange func(step string)
}

// Result — итог работы.
type Result struct {
	Status   string `json:"status"`
	Summary  string `json:"summary"`
	Model    string `json:"model"`    // модель, которая справилась
	Baseline string `json:"baseline"` // снимок до начала работы
	Final    string `json:"final"`    // снимок готового результата
	Diff     string `json:"diff"`
}

func (t *Task) emit(kind, format string, args ...any) {
	if t.OnEvent != nil {
		t.OnEvent(Event{At: time.Now(), Kind: kind, Text: fmt.Sprintf(format, args...)})
	}
}

const systemPrompt = `Ты — агент-разработчик в Edy. Ты работаешь в папке проекта и меняешь код только через инструменты.

Как работать:
1. Посмотри скелет проекта (project_skeleton): файлы, типы и функции с номерами строк.
   Нужный код ищи через search_code, файлы целиком читай (read_file) только когда это необходимо.
2. Внеси изменения: replace_in_file для точечных правок, write_file для новых файлов.
3. Когда задача выполнена, вызови finish с кратким итогом.

После finish Edy сам запустит проверки (линтеры и тесты). Если они упадут, ты получишь вывод и должен исправить ошибки.
Не трогай файлы, не связанные с задачей. Пиши код в стиле проекта. Отвечай по-русски.`

// Run выполняет задачу. Проект должен быть git-репозиторием без
// незакоммиченных изменений: так результат можно чисто оформить в ветку.
func (t *Task) Run(ctx context.Context) (Result, error) {
	if t.MaxAttempts <= 0 {
		t.MaxAttempts = 3
	}
	if t.MaxSteps <= 0 {
		t.MaxSteps = 40
	}
	if len(t.Models) == 0 {
		return Result{Status: StatusFailed}, errors.New("не указана ни одна модель")
	}
	if err := CheckClean(ctx, t.Root); err != nil {
		return Result{Status: StatusFailed}, err
	}
	if isRepo(ctx, t.Root) {
		if err := excludeEdyDir(t.Root); err != nil {
			return Result{Status: StatusFailed}, err
		}
	} else {
		t.emit("info", "Папка без git: снимки делаются в Shadow Git, результат останется в папке")
	}
	hooks, err := DetectHooks(t.Root)
	if err != nil {
		return Result{Status: StatusFailed}, err
	}
	if len(hooks) == 0 {
		t.emit("info", "Проверки не найдены (нет go.mod, ruff/pytest или %s) — результат примется без тестов", HooksFile)
	} else {
		names := make([]string, len(hooks))
		for i, h := range hooks {
			names[i] = h.Name
		}
		t.emit("info", "Проверки после работы: %s", strings.Join(names, ", "))
	}

	shadow := Shadow{Root: t.Root}
	if err := shadow.Init(ctx); err != nil {
		return Result{Status: StatusFailed}, err
	}
	baseline, err := shadow.Snapshot(ctx, "Начало задачи: "+firstLine(t.Goal))
	if err != nil {
		return Result{Status: StatusFailed}, err
	}
	t.emit("checkpoint", "Снимок №0 — исходное состояние")

	// CLI-утилиты запускаются в папке проекта.
	ctx = WithCallOptions(ctx, CallOptions{Workdir: t.Root})
	ws := Workspace{Root: t.Root, Skills: t.Skills, Memory: t.Memory, Code: t.Code}
	if len(t.Skills) > 0 {
		t.emit("info", "Скиллов доступно агенту: %d", len(t.Skills))
	}
	res := Result{Baseline: baseline}
	step := 0
	for mi, model := range t.Models {
		if mi > 0 {
			t.emit("breaker", "Circuit Breaker: проект откачен к исходному состоянию, задача передана модели %s", model)
		}
		if t.OnAttempt != nil {
			t.OnAttempt(model)
		}
		var summary string
		var ok bool
		if t.MCP != nil && t.MCP.Supports(model) {
			summary, ok, err = t.attemptMCP(ctx, model, hooks, &step)
		} else {
			if len(t.Extra) > 0 {
				t.emit("info", "%s не умеет MCP: серверы %s не выданы, работает через инструменты Edy в промпте", model, strings.Join(t.Extra, ", "))
			}
			summary, ok, err = t.attempt(ctx, model, ws, shadow, hooks, &step)
		}
		if t.OnAttemptEnd != nil {
			note := summary
			switch {
			case err != nil:
				note = err.Error()
			case t.refused != "":
				note = "отказалась работать: " + t.refused
			case !ok:
				note = "не прошла проверки — задача передана дальше"
			}
			t.refused = ""
			t.OnAttemptEnd(model, ok && err == nil, note)
		}
		if err != nil {
			return Result{Status: StatusFailed, Baseline: baseline}, err
		}
		if ok {
			final, err := shadow.Snapshot(ctx, "Готово: "+firstLine(summary))
			if err != nil {
				return Result{Status: StatusFailed, Baseline: baseline}, err
			}
			res.Status, res.Summary, res.Model, res.Final = StatusDone, summary, model, final
			res.Diff, _ = shadow.Diff(ctx, baseline)
			t.emit("info", "Готово (%s). Проверьте изменения и нажмите Approve.", model)
			return res, nil
		}
		// Слабая модель не справилась — откатываем её правки целиком.
		if err := shadow.Restore(ctx, baseline); err != nil {
			return Result{Status: StatusFailed, Baseline: baseline}, err
		}
	}
	t.emit("breaker", "Ни одна модель не справилась за %d попытки — проект откачен, нужна помощь человека", t.MaxAttempts)
	res.Status = StatusNeedsHuman
	return res, nil
}

// attempt — работа одной модели: до MaxAttempts провалов проверок подряд.
func (t *Task) attempt(ctx context.Context, model string, ws Workspace, shadow Shadow, hooks []Hook, step *int) (string, bool, error) {
	msgs := []Msg{
		{Role: "system", Content: strings.TrimSpace(systemPrompt + "\n\n" + skills.Catalog(t.Skills) + t.memoryNote())},
		{Role: "user", Content: "Задача:\n" + t.Goal, Images: t.imagesFor(model)},
	}
	tools := ws.Tools()
	failures := 0
	steps := 0
	maxWait := t.MaxWait
	if maxWait == 0 {
		maxWait = 5 * time.Minute
	}
	var waited time.Duration
	for {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		if steps >= t.MaxSteps {
			t.emit("error", "%s: превышено %d шагов без finish", model, t.MaxSteps)
			return "", false, nil
		}
		steps++
		reply, err := t.LLM.Chat(ctx, model, msgs, tools)
		if err != nil {
			if wait, limited := RateLimited(err); limited && wait > 0 && wait <= maxRetryAfter && waited+wait <= maxWait {
				// Минутный лимит: ждём сброса и повторяем тот же шаг, попытку не тратим.
				t.emit("info", "%s упёрлась в лимит в минуту — жду %s и продолжаю", model, wait.Round(time.Second))
				select {
				case <-ctx.Done():
					return "", false, ctx.Err()
				case <-time.After(wait):
				}
				waited += wait
				steps--
				continue
			}
			t.emit("error", "%v", err)
			if Hopeless(err) {
				t.refused = err.Error()
				return "", false, nil // сразу к резервной модели
			}
			failures++
			if failures >= t.MaxAttempts {
				return "", false, nil
			}
			continue
		}
		waited = 0 // модель ответила — ожидание засчитано как прогресс
		msgs = append(msgs, reply)
		if strings.TrimSpace(reply.Content) != "" {
			t.emit("model", "%s: %s", model, lastRunes(strings.TrimSpace(reply.Content), 600))
		}
		if len(reply.ToolCalls) == 0 {
			msgs = append(msgs, Msg{Role: "user", Content: "Используй инструменты, чтобы выполнить задачу. Когда закончишь — вызови finish."})
			continue
		}

		finished, summary := false, ""
		for _, tc := range reply.ToolCalls {
			name, args := tc.Function.Name, tc.Function.Arguments
			if name == "finish" {
				finished = true
				summary = argString(args, "summary")
				msgs = append(msgs, Msg{Role: "tool", ToolCallID: tc.ID, Content: "Принято, запускаю проверки."})
				continue
			}
			out, changed, err := ws.Call(name, args)
			if err != nil {
				out = "Ошибка: " + err.Error()
			}
			t.emit("tool", "%s %s → %s", name, argString(args, "path"), lastRunes(firstLine(out), 200))
			msgs = append(msgs, Msg{Role: "tool", ToolCallID: tc.ID, Content: out})
			if changed {
				*step++
				if _, err := shadow.Snapshot(ctx, fmt.Sprintf("Шаг %d: %s %s", *step, name, argString(args, "path"))); err != nil {
					return "", false, err
				}
				t.emit("checkpoint", "Снимок №%d — %s %s", *step, name, argString(args, "path"))
			}
		}
		if !finished {
			continue
		}

		ok, report := RunHooks(ctx, t.Root, hooks)
		t.emit("hooks", "%s", strings.TrimSpace(report))
		if ok {
			return summary, true, nil
		}
		failures++
		if failures >= t.MaxAttempts {
			t.emit("breaker", "%s провалила проверки %d раза подряд", model, failures)
			return "", false, nil
		}
		msgs = append(msgs, Msg{Role: "user", Content: fmt.Sprintf("Проверки не прошли (попытка %d из %d). Исправляй:\n\n%s", failures, t.MaxAttempts, report)})
	}
}

// imagesFor — скриншоты задачи, если модель их видит.
func (t *Task) imagesFor(model string) []string {
	if len(t.Images) == 0 {
		return nil
	}
	if t.CanSee == nil || !t.CanSee(model) {
		t.emit("info", "К задаче приложено скриншотов: %d, но %s не видит картинки — работает только по тексту", len(t.Images), model)
		return nil
	}
	t.emit("info", "%s получает скриншоты задачи: %d", model, len(t.Images))
	return t.Images
}

// memoryNote — раздел системного промпта о памяти проекта.
func (t *Task) memoryNote() string {
	role := ""
	if strings.TrimSpace(t.Role) != "" {
		role = "\n\n" + strings.TrimSpace(t.Role)
	}
	if t.Memory == nil {
		return role
	}
	note := "\n\nПамять проекта: ищи в ней через project_memory (как запускать, что хрупко, уроки прошлых задач); полезный неочевидный факт запиши через remember."
	if strings.TrimSpace(t.MemoryBrief) != "" {
		note += "\nЗакреплённое в памяти проекта:\n" + t.MemoryBrief
	}
	return role + note
}

const mcpPrompt = `Ты — агент-разработчик в Edy. Работай с проектом через инструменты MCP-сервера edy:
project_skeleton и search_code — чтобы разобраться в коде, read_file — прочитать файл,
replace_in_file и write_file — править, delete_file — удалять, run_checks — запустить линтеры и тесты,
list_skills и use_skill — инструкции проекта. Встроенные инструменты правки файлов и терминал отключены.

Как работать:
1. Посмотри скелет проекта, найди нужный код.
2. Внеси изменения. Не трогай файлы, не связанные с задачей. Пиши код в стиле проекта.
3. Вызови run_checks и исправь ошибки, пока проверки не пройдут.
4. В конце ответь кратким итогом: что сделано, 1–3 предложения. Отвечай по-русски.`

// attemptMCP — работа исполнителя, подключённого к MCP Edy: утилита сама
// ведёт цикл и правит проект инструментами Edy, а Edy после её ответа
// запускает проверки. До MaxAttempts провалов подряд.
func (t *Task) attemptMCP(ctx context.Context, model string, hooks []Hook, step *int) (string, bool, error) {
	var mu sync.Mutex
	names := make([]string, len(t.Skills))
	for i, sk := range t.Skills {
		names[i] = sk.Name
	}
	url, release := t.MCP.Bind(t.Root, MCPEvents{
		Skills: names,
		Memory: t.Memory,
		Code:   t.Code,
		OnCall: func(tool, note string, isErr bool) {
			kind := "tool"
			if isErr {
				kind = "error"
			}
			t.emit(kind, "%s %s", tool, note)
		},
		OnChange: func(s string) {
			mu.Lock()
			*step++
			n := *step
			mu.Unlock()
			t.emit("checkpoint", "Снимок №%d — %s", n, strings.TrimPrefix(s, "MCP: "))
		},
	})
	defer release()
	if len(t.Extra) > 0 {
		t.emit("info", "%s получает MCP: Edy, %s (выдал Оркестратор). Правки через Edy — снимки", model, strings.Join(t.Extra, ", "))
	} else {
		t.emit("info", "%s получает MCP Edy: правит проект инструментами Edy, каждая правка — снимок", model)
	}
	ctx = WithCallOptions(ctx, CallOptions{Workdir: t.Root, MCP: url, MCPServers: t.Extra})

	base := strings.TrimSpace(mcpPrompt+"\n\n"+skills.Catalog(t.Skills)+t.memoryNote()) + "\n\nЗадача:\n" + t.Goal
	if len(t.Extra) > 0 {
		base = strings.TrimSpace(mcpPrompt+"\n\n"+skills.Catalog(t.Skills)+t.memoryNote()) +
			"\n\nДля этой задачи тебе также выданы MCP-серверы: " + strings.Join(t.Extra, ", ") +
			". Пользуйся ими, когда они нужны (например, проверить результат), но правь файлы проекта только через edy.\n\nЗадача:\n" + t.Goal
	}
	prompt := base
	failures := 0
	for {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		reply, err := t.LLM.Chat(ctx, model, []Msg{{Role: "user", Content: prompt, Images: t.imagesFor(model)}}, nil)
		if err != nil {
			if ctx.Err() != nil {
				return "", false, ctx.Err()
			}
			t.emit("error", "%v", err)
			failures++
			if failures >= t.MaxAttempts {
				return "", false, nil
			}
			continue
		}
		summary := lastRunes(strings.TrimSpace(reply.Content), 600)
		if summary != "" {
			t.emit("model", "%s: %s", model, summary)
		}
		ok, report := RunHooks(ctx, t.Root, hooks)
		t.emit("hooks", "%s", strings.TrimSpace(report))
		if ok {
			return summary, true, nil
		}
		failures++
		if failures >= t.MaxAttempts {
			t.emit("breaker", "%s провалила проверки %d раза подряд", model, failures)
			return "", false, nil
		}
		prompt = base + fmt.Sprintf("\n\nПредыдущая попытка уже внесла правки в проект, но проверки не прошли (попытка %d из %d). Исправь:\n\n%s", failures, t.MaxAttempts, report)
	}
}

func argString(args, key string) string {
	var m map[string]any
	if json := strings.TrimSpace(args); json == "" || unmarshal(json, &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ---- Основной репозиторий проекта ----

func gitIn(ctx context.Context, root string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func isRepo(ctx context.Context, root string) bool {
	_, err := gitIn(ctx, root, "rev-parse", "--is-inside-work-tree")
	return err == nil
}

// CheckClean проверяет, что проект готов к задаче: папка без git подходит всегда,
// а git-репозиторий должен быть без незакоммиченных изменений.
func CheckClean(ctx context.Context, root string) error {
	if _, err := exec.LookPath("git"); err != nil {
		return errors.New("не найден git (он нужен и для снимков) — установите Xcode Command Line Tools: xcode-select --install")
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return fmt.Errorf("папка %s не найдена", root)
	}
	if !isRepo(ctx, root) {
		return nil
	}
	if _, err := gitIn(ctx, root, "rev-parse", "HEAD"); err != nil {
		return errors.New("в репозитории ещё нет коммитов — сделайте первый коммит")
	}
	// Служебная папка Edy (снимки, скиллы проекта) не считается изменением.
	if err := excludeEdyDir(root); err != nil {
		return err
	}
	status, err := gitIn(ctx, root, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("в проекте есть незакоммиченные изменения — закоммитьте или уберите их, чтобы результат агента лёг в отдельную ветку чисто")
	}
	return nil
}

// Branch возвращает текущую ветку проекта.
func Branch(ctx context.Context, root string) (string, error) {
	return gitIn(ctx, root, "rev-parse", "--abbrev-ref", "HEAD")
}

// excludeEdyDir прячет .edy от основного git, не меняя .gitignore проекта.
func excludeEdyDir(root string) error {
	gitDir, err := gitIn(context.Background(), root, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	path := filepath.Join(gitDir, "info", "exclude")
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), ".edy/") {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		data = append(data, '\n')
	}
	return os.WriteFile(path, append(data, []byte(".edy/\n")...), 0o644)
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// BranchName делает имя ветки edy/<задача> из текста задачи.
func BranchName(goal string, now time.Time) string {
	slug := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(translit(firstLine(goal))), "-"), "-")
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	if slug == "" {
		slug = "task"
	}
	return "edy/" + slug + "-" + now.Format("0102-1504")
}

// HasChanges сообщает, есть ли в проекте незакоммиченные изменения (в папке
// без git — всегда да: проверить нечем).
func HasChanges(ctx context.Context, root string) bool {
	if !isRepo(ctx, root) {
		return true
	}
	status, err := gitIn(ctx, root, "status", "--porcelain")
	return err != nil || status != ""
}

// FreeBranch возвращает name, а если такая ветка уже есть — name-2, name-3…
// (две задачи с одинаковым названием, принятые в одну минуту).
func FreeBranch(ctx context.Context, root, name string) string {
	branch := name
	for i := 2; i < 100; i++ {
		if _, err := gitIn(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err != nil {
			return branch
		}
		branch = fmt.Sprintf("%s-%d", name, i)
	}
	return branch
}

// Approve оформляет результат одним коммитом в новой ветке и переключает на неё проект.
func Approve(ctx context.Context, root, branch, message string) error {
	if _, err := gitIn(ctx, root, "check-ref-format", "--branch", branch); err != nil {
		return fmt.Errorf("некорректное имя ветки %q", branch)
	}
	if _, err := gitIn(ctx, root, "checkout", "-q", "-b", branch); err != nil {
		return err
	}
	if _, err := gitIn(ctx, root, "add", "-A"); err != nil {
		return err
	}
	_, err := gitIn(ctx, root, "commit", "-q", "-m", message)
	return err
}

// Reject откатывает проект к состоянию до задачи.
func Reject(ctx context.Context, root, baseline string) error {
	return Shadow{Root: root}.Restore(ctx, baseline)
}

var translitMap = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e", 'ж': "zh", 'з': "z", 'и': "i", 'й': "y",
	'к': "k", 'л': "l", 'м': "m", 'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u", 'ф': "f",
	'х': "h", 'ц': "c", 'ч': "ch", 'ш': "sh", 'щ': "sch", 'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya",
}

func translit(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if t, ok := translitMap[r]; ok {
			b.WriteString(t)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
