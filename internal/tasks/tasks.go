// Package tasks — доска задач проекта: пул идей, очередь, выполнение агентом
// в фоне, ревью и решение Approve/Reject. Задачи и их история хранятся в
// SQLite, поэтому переживают перезапуск Edy.
package tasks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/laurenwilcox/name/internal/agent"
	"github.com/laurenwilcox/name/internal/skills"
	"github.com/laurenwilcox/name/internal/store"
)

// Journal — журнал агентов (SQLite). Может быть nil.
type Journal interface {
	CreateAgent(a store.Agent) error
	AddAgentEvent(agentID, kind, text string, at time.Time) error
	FinishAgent(id, state, summary, errText string) error
}

// Board — хранилище карточек (SQLite).
type Board interface {
	SaveBoardTask(t store.BoardTask) error
	BoardTask(id string) (store.BoardTask, error)
	BoardTasks(project string) ([]store.BoardTask, error)
	DeleteBoardTask(id string) error
	AddBoardEvent(taskID, kind, text string, at time.Time) error
	BoardEvents(taskID string, afterID int64) ([]store.BoardEvent, error)
}

// Outcome — что сделано с результатом при Approve.
type Outcome struct {
	Branch string `json:"branch,omitempty"`
	PRURL  string `json:"pr_url,omitempty"`
	Note   string `json:"note"`
}

// FinalizeFunc оформляет результат по настройкам проекта (коммит, push, PR).
type FinalizeFunc func(ctx context.Context, project, goal, summary, model string) (Outcome, error)

// Reviewer читает изменения готовой задачи (patch — полный diff) и пишет
// замечания: второй агент, по возможности на другой модели. Вернуть пустой
// Review{} без ошибки — ревью для проекта выключено.
type Reviewer func(ctx context.Context, v View, patch string) (Review, error)

// Review — итог ревью.
type Review struct {
	Model   string `json:"model"`
	Verdict string `json:"verdict"` // ok — замечаний нет, issues — есть замечания, error — ревью не удалось
	Text    string `json:"text"`
}

// Состояния задачи (они же колонки доски).
const (
	StateBacklog    = "backlog" // пул: идея, ещё не поставлена в работу
	StateQueued     = "queued"  // ждёт своей очереди
	StateRunning    = "running"
	StateDone       = "done"        // проверки прошли, ждём решения
	StateNeedsHuman = "needs_human" // ни одна модель не справилась, проект откачен
	StateFailed     = "failed"
	StateCancelled  = "cancelled"
	StateApproved   = "approved"
	StateRejected   = "rejected"
)

var ErrBusy = errors.New("в этом проекте уже идёт задача — дождитесь её или остановите")

// View — карточка задачи для интерфейса.
type View struct {
	ID       string             `json:"id"`
	Project  string             `json:"project"`
	Parent   string             `json:"parent,omitempty"` // подзадача: к какой задаче относится
	Title    string             `json:"title"`
	Request  string             `json:"request,omitempty"` // исходная просьба
	Plan     string             `json:"plan,omitempty"`    // утверждённый план Оркестратора
	Goal     string             `json:"goal"`              // что получает исполнитель
	Models   []string           `json:"models"`
	MCP      []string           `json:"mcp,omitempty"`    // внешние MCP-серверы, выданные Оркестратором
	Skills   []string           `json:"skills,omitempty"` // скиллы, выданные исполнителю
	State    string             `json:"state"`
	Error    string             `json:"error,omitempty"`
	Summary  string             `json:"summary,omitempty"`
	Model    string             `json:"model,omitempty"`
	Diff     string             `json:"diff,omitempty"`
	Review   *Review            `json:"review,omitempty"`
	Branch   string             `json:"branch,omitempty"`
	PRURL    string             `json:"pr_url,omitempty"`
	Note     string             `json:"note,omitempty"`
	Agents   []string           `json:"agents"` // id агентов в журнале
	Position int64              `json:"position"`
	Created  time.Time          `json:"created"`
	Updated  time.Time          `json:"updated"`
	Events   []store.BoardEvent `json:"events"`
	NextFrom int64              `json:"next_from"` // id события, после которого запрашивать новые
}

// record — то, что хранится в базе: карточка и служебные поля.
type record struct {
	View
	Baseline  string `json:"baseline,omitempty"`
	AllSkills bool   `json:"all_skills,omitempty"` // задача без Оркестратора получает все скиллы
}

type run struct {
	id      string
	cancel  context.CancelFunc
	agentID string        // текущий агент-исполнитель
	pending []agent.Event // события до появления первого агента
}

// Manager ведёт доску задач и запускает их по очереди: в одном проекте
// одновременно работает одна задача, следующая стартует после решения по
// предыдущей (или сразу, если включено автопринятие).
type Manager struct {
	LLM      agent.LLM
	Board    Board
	Journal  Journal
	Finalize FinalizeFunc // nil — новая ветка edy/… без push
	Review   Reviewer     // nil — без ревью
	// AutoApprove сообщает, принимать ли результат в проекте без человека
	// (если проверки и ревью прошли). nil — никогда.
	AutoApprove func(project string) bool
	// Notify сообщает пользователю о важном: готово, нужна помощь, ошибка.
	Notify func(v View, message string)
	// SkillsHome — папка данных Edy: общие скиллы лежат в её skills/.
	SkillsHome string
	// MCP подключает MCP-сервер Edy к исполнителям, которые это умеют.
	MCP agent.MCPBridge
	// Now подменяется в тестах.
	Now func() time.Time

	mu      sync.Mutex
	active  map[string]*run // проект → выполняющаяся задача
	running sync.WaitGroup
}

func NewManager(llm agent.LLM, board Board) *Manager {
	return &Manager{LLM: llm, Board: board, Now: time.Now, active: map[string]*run{}}
}

// ---- хранение ----

func (m *Manager) load(id string) (record, error) {
	t, err := m.Board.BoardTask(id)
	if errors.Is(err, store.ErrNotFound) {
		return record{}, errors.New("задача не найдена")
	} else if err != nil {
		return record{}, err
	}
	var r record
	if err := json.Unmarshal(t.Data, &r); err != nil {
		return record{}, fmt.Errorf("задача %s повреждена: %w", id, err)
	}
	r.ID, r.Project, r.Parent, r.Title, r.State, r.Position = t.ID, t.Project, t.Parent, t.Title, t.State, t.Position
	r.Created, r.Updated = t.Created, t.Updated
	if r.Agents == nil {
		r.Agents = []string{}
	}
	return r, nil
}

func (m *Manager) save(r *record) error {
	r.Updated = m.Now()
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return m.Board.SaveBoardTask(store.BoardTask{ID: r.ID, Project: r.Project, Parent: r.Parent, Title: r.Title,
		Data: data, State: r.State, Position: r.Position, Created: r.Created, Updated: r.Updated})
}

// event пишет запись в историю карточки (и в журнал агента, если он есть).
func (m *Manager) event(id, agentID, kind, text string) {
	at := m.Now()
	m.Board.AddBoardEvent(id, kind, text, at)
	if agentID != "" && m.Journal != nil {
		m.Journal.AddAgentEvent(agentID, kind, text, at)
	}
}

// update загружает задачу, меняет её и сохраняет под общей блокировкой.
func (m *Manager) update(id string, fn func(r *record) error) (record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.load(id)
	if err != nil {
		return record{}, err
	}
	if err := fn(&r); err != nil {
		return record{}, err
	}
	return r, m.save(&r)
}

// ---- создание и очередь ----

// Draft — новая карточка.
type Draft struct {
	Project string
	Parent  string
	Title   string
	Request string
}

// Create кладёт задачу в пул (колонка «Пул задач»), без запуска.
func (m *Manager) Create(d Draft) (View, error) {
	if strings.TrimSpace(d.Request) == "" && strings.TrimSpace(d.Title) == "" {
		return View{}, errors.New("опишите задачу")
	}
	if d.Parent != "" {
		p, err := m.load(d.Parent)
		if err != nil {
			return View{}, err
		}
		if p.Project != d.Project {
			return View{}, errors.New("подзадача должна быть в том же проекте")
		}
	}
	r := record{View: View{ID: newID(), Project: d.Project, Parent: d.Parent, Title: titleOf(d.Title, d.Request),
		Request: strings.TrimSpace(d.Request), State: StateBacklog, Created: m.Now(), Position: m.Now().UnixMilli(), Agents: []string{}}}
	m.mu.Lock()
	err := m.save(&r)
	m.mu.Unlock()
	if err != nil {
		return View{}, err
	}
	if d.Parent != "" {
		m.event(d.Parent, "", "subtask", "Добавлена подзадача: "+r.Title)
		m.event(r.ID, "", "created", "Подзадача к «"+m.titleOf(d.Parent)+"»")
	} else {
		m.event(r.ID, "", "created", "Задача добавлена в пул")
	}
	return m.Get(r.ID, 0), nil
}

func (m *Manager) titleOf(id string) string {
	r, err := m.load(id)
	if err != nil {
		return id
	}
	return r.Title
}

// Options — что Оркестратор выдал исполнителю.
type Options struct {
	// MCP — внешние MCP-серверы из каталога (только исполнителю с MCP).
	MCP []string
	// Skills — выданные скиллы: nil — все скиллы (задача без Оркестратора),
	// пустой список — никаких.
	Skills []string
}

// Plan — что нужно, чтобы поставить задачу в очередь.
type Plan struct {
	Request string
	Plan    string
	Goal    string
	Models  []string
	Options
}

// Enqueue ставит задачу из пула (или новую, если id пустой) в очередь
// проекта с утверждённым планом и моделями. Если проект свободен, задача
// сразу начинается.
func (m *Manager) Enqueue(id string, d Draft, p Plan) (View, error) {
	if strings.TrimSpace(p.Goal) == "" || len(p.Models) == 0 {
		return View{}, errors.New("нужны задача и хотя бы одна модель")
	}
	if id == "" {
		v, err := m.Create(Draft{Project: d.Project, Parent: d.Parent, Title: d.Title, Request: firstNonEmpty(p.Request, d.Request, p.Goal)})
		if err != nil {
			return View{}, err
		}
		id = v.ID
	}
	r, err := m.update(id, func(r *record) error {
		switch r.State {
		case StateBacklog, StateNeedsHuman, StateFailed, StateCancelled, StateRejected:
		default:
			return fmt.Errorf("задача в состоянии %q, поставить её в очередь нельзя", r.State)
		}
		r.State, r.Goal, r.Models, r.MCP = StateQueued, p.Goal, p.Models, p.MCP
		if p.Request != "" {
			r.Request = p.Request
		}
		r.Plan = p.Plan
		r.Skills, r.AllSkills = p.Skills, p.Skills == nil
		r.Error, r.Summary, r.Diff, r.Review, r.Model = "", "", "", nil, ""
		r.Position = m.Now().UnixMilli()
		return nil
	})
	if err != nil {
		return View{}, err
	}
	if r.Plan != "" {
		m.event(id, "", "plan", r.Plan)
	}
	m.event(id, "", "queued", "В очереди. Исполнитель: "+strings.Join(r.Models, " → "))
	m.kick(r.Project)
	return m.Get(id, 0), nil
}

// StartWith — задача сразу в очередь (для задач без пула: edy task, API).
func (m *Manager) StartWith(project, goal string, models []string, o Options) (View, error) {
	if err := agent.CheckClean(context.Background(), project); err != nil && !m.Running(project) {
		return View{}, err
	}
	return m.Enqueue("", Draft{Project: project}, Plan{Goal: goal, Models: models, Options: o})
}

// Start — StartWith со всеми скиллами.
func (m *Manager) Start(project, goal string, models []string) (View, error) {
	return m.StartWith(project, goal, models, Options{})
}

// kick запускает следующую задачу из очереди, если проект свободен: нет
// работающей задачи и нет результата, ждущего решения.
func (m *Manager) kick(project string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active[project] != nil {
		return
	}
	list, err := m.Board.BoardTasks(project)
	if err != nil {
		return
	}
	var next *store.BoardTask
	for i := range list {
		switch list[i].State {
		case StateDone:
			return // ждём Approve/Reject
		case StateQueued:
			if next == nil {
				next = &list[i]
			}
		}
	}
	if next == nil {
		return
	}
	r, err := m.load(next.ID)
	if err != nil {
		return
	}
	if err := agent.CheckClean(context.Background(), project); err != nil {
		// Очередь останавливается: следующие задачи упадут по той же причине.
		r.State, r.Error = StateFailed, err.Error()
		m.save(&r)
		go m.event(r.ID, "", "error", "Не удалось начать: "+err.Error())
		go m.notify(r.View, "Задача не началась: "+err.Error())
		return
	}
	list2, err := skills.Load(skills.Dirs(m.SkillsHome, project))
	if err != nil {
		list2 = nil
	}
	if !r.AllSkills {
		given := []skills.Skill{}
		for _, name := range r.Skills {
			if sk, ok := skills.Find(list2, name); ok {
				given = append(given, sk)
			}
		}
		list2 = given
	}
	r.Skills = nil
	for _, sk := range list2 {
		r.Skills = append(r.Skills, sk.Name)
	}
	r.State = StateRunning
	if err := m.save(&r); err != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	ru := &run{id: r.ID, cancel: cancel}
	m.active[project] = ru
	m.running.Add(1)
	go m.work(ctx, ru, r, list2)
}

// ---- выполнение ----

func (m *Manager) work(ctx context.Context, ru *run, r record, given []skills.Skill) {
	defer m.running.Done()
	m.event(r.ID, "", "started", "Работа началась")
	attempt := 0
	var agentsMu sync.Mutex
	task := &agent.Task{
		Root:   r.Project,
		Goal:   r.Goal,
		Models: r.Models,
		LLM:    m.LLM,
		Skills: given,
		MCP:    m.MCP,
		Extra:  r.MCP,
		OnEvent: func(e agent.Event) {
			agentsMu.Lock()
			id := ru.agentID
			if id == "" {
				ru.pending = append(ru.pending, e)
			}
			agentsMu.Unlock()
			m.Board.AddBoardEvent(r.ID, e.Kind, e.Text, e.At)
			if id != "" && m.Journal != nil {
				m.Journal.AddAgentEvent(id, e.Kind, e.Text, e.At)
			}
		},
		// Каждая модель, взявшая задачу, — отдельный агент в журнале.
		OnAttempt: func(model string) {
			attempt++
			id := fmt.Sprintf("%s-%d", r.ID, attempt)
			if m.Journal != nil {
				m.Journal.CreateAgent(store.Agent{ID: id, Role: store.RoleExecutor, Model: model, Project: r.Project, TaskID: r.ID, Goal: r.Goal, Created: m.Now()})
			}
			if attempt > 1 {
				m.event(r.ID, "", "model", "Модель сменилась: задачу взяла "+model)
			}
			agentsMu.Lock()
			ru.agentID = id
			pending := ru.pending
			ru.pending = nil
			agentsMu.Unlock()
			m.update(r.ID, func(x *record) error { x.Agents = append(x.Agents, id); return nil })
			if m.Journal != nil {
				for _, e := range pending {
					m.Journal.AddAgentEvent(id, e.Kind, e.Text, e.At)
				}
			}
		},
		OnAttemptEnd: func(model string, ok bool, note string) {
			agentsMu.Lock()
			id := ru.agentID
			agentsMu.Unlock()
			if m.Journal == nil || id == "" {
				return
			}
			switch {
			case ok:
				m.Journal.FinishAgent(id, store.AgentDone, note, "")
			case ctx.Err() != nil:
				m.Journal.FinishAgent(id, store.AgentCancelled, "", "остановлен пользователем")
			default:
				m.Journal.FinishAgent(id, store.AgentFailed, "", note)
			}
		},
	}
	res, err := task.Run(ctx)

	agentsMu.Lock()
	agentID := ru.agentID
	agentsMu.Unlock()
	final, _ := m.update(r.ID, func(x *record) error {
		x.Baseline = res.Baseline
		switch {
		case ctx.Err() != nil:
			x.State = StateCancelled
			if x.Baseline != "" {
				// Остановленная на полпути работа откатывается целиком.
				if err := agent.Reject(context.Background(), x.Project, x.Baseline); err != nil {
					x.Error = "не удалось откатить: " + err.Error()
				}
			}
		case err != nil:
			x.State, x.Error = StateFailed, err.Error()
		case res.Status == agent.StatusDone:
			x.State, x.Summary, x.Model, x.Diff = StateDone, res.Summary, res.Model, res.Diff
		default:
			x.State = StateNeedsHuman
		}
		return nil
	})
	switch final.State {
	case StateCancelled:
		m.event(r.ID, agentID, "info", "Задача остановлена, проект возвращён в исходное состояние")
	case StateFailed:
		m.event(r.ID, agentID, "error", final.Error)
		m.notify(final.View, "Ошибка: "+final.Error)
	case StateNeedsHuman:
		m.notify(final.View, "Нужна помощь: ни одна модель не справилась, проект откачен")
	case StateDone:
		final = m.afterDone(final, agentID)
	}

	m.mu.Lock()
	delete(m.active, r.Project)
	m.mu.Unlock()
	m.kick(r.Project)
}

// afterDone — ревью и, если включено, автопринятие.
func (m *Manager) afterDone(r record, agentID string) record {
	if rv := m.review(r, agentID); rv != nil {
		r, _ = m.update(r.ID, func(x *record) error { x.Review = rv; return nil })
	}
	if m.AutoApprove != nil && m.AutoApprove(r.Project) && (r.Review == nil || r.Review.Verdict == "ok") {
		m.event(r.ID, agentID, "info", "Автопринятие: проверки и ревью прошли")
		v, err := m.approve(r.ID, true)
		if err == nil {
			m.notify(v, "Готово и принято автоматически: "+v.Note)
			r.View = v
			return r
		}
		m.event(r.ID, agentID, "error", "Автопринятие не удалось: "+err.Error())
	}
	msg := "Готово — проверьте и примите (Approve) или отклоните"
	if r.Review != nil && r.Review.Verdict == "issues" {
		msg = "Готово, но у ревьюера есть замечания — посмотрите перед Approve"
	}
	m.notify(r.View, msg)
	return r
}

// review запускает ревьюера; nil — ревью нет (не настроено или выключено).
func (m *Manager) review(r record, agentID string) *Review {
	if m.Review == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	patch, err := agent.Shadow{Root: r.Project}.Patch(ctx, r.Baseline, 60<<10)
	var rv Review
	if err == nil {
		rv, err = m.Review(ctx, r.View, patch)
	}
	switch {
	case err != nil:
		rv = Review{Verdict: "error", Text: err.Error(), Model: rv.Model}
	case rv.Verdict == "":
		return nil // ревью выключено в настройках проекта
	}
	switch rv.Verdict {
	case "ok":
		m.event(r.ID, agentID, "review", "Ревью ("+rv.Model+"): замечаний нет")
	case "issues":
		m.event(r.ID, agentID, "review", "Ревью ("+rv.Model+"): есть замечания\n"+rv.Text)
	default:
		m.event(r.ID, agentID, "review", "Ревью не удалось: "+rv.Text)
	}
	return &rv
}

func (m *Manager) notify(v View, msg string) {
	if m.Notify != nil {
		m.Notify(v, msg)
	}
}

// ---- чтение ----

// Get возвращает задачу с событиями после события from.
func (m *Manager) Get(id string, from int64) View {
	r, err := m.load(id)
	if err != nil {
		return View{}
	}
	v := r.View
	v.Events, _ = m.Board.BoardEvents(id, from)
	if v.Events == nil {
		v.Events = []store.BoardEvent{}
	}
	v.NextFrom = from
	if n := len(v.Events); n > 0 {
		v.NextFrom = v.Events[n-1].ID
	}
	return v
}

// List возвращает задачи проекта (project пустой — все) без событий.
func (m *Manager) List(project string) []View {
	list, err := m.Board.BoardTasks(project)
	if err != nil {
		return []View{}
	}
	out := make([]View, 0, len(list))
	for _, t := range list {
		if r, err := m.load(t.ID); err == nil {
			r.Diff = "" // большой; берётся из карточки
			out = append(out, r.View)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Position < out[j].Position })
	return out
}

// Running сообщает, идёт ли задача в проекте.
func (m *Manager) Running(project string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active[project] != nil
}

// Pending сообщает, ждёт ли в проекте готовый результат решения (Approve/Reject).
func (m *Manager) Pending(project string) bool {
	list, _ := m.Board.BoardTasks(project)
	for _, t := range list {
		if t.State == StateDone {
			return true
		}
	}
	return false
}

// ---- решения пользователя ----

// Approve оформляет результат по настройкам проекта (по умолчанию — один
// коммит в новой ветке edy/…) и запускает следующую задачу из очереди.
func (m *Manager) Approve(id string) (View, error) { return m.approve(id, false) }

func (m *Manager) approve(id string, auto bool) (View, error) {
	r, err := m.load(id)
	if err != nil {
		return View{}, err
	}
	if r.State != StateDone {
		return View{}, fmt.Errorf("задача в состоянии %q, решение уже не требуется", r.State)
	}
	finalize := m.Finalize
	if finalize == nil {
		finalize = defaultFinalize(m.Now)
	}
	var out Outcome
	if agent.HasChanges(context.Background(), r.Project) {
		if out, err = finalize(context.Background(), r.Project, r.Goal, r.Summary, r.Model); err != nil {
			return View{}, err
		}
	} else {
		// Задача ничего не поменяла (например, всё уже было сделано): коммитить нечего.
		out = Outcome{Note: "изменений нет — коммит не нужен"}
	}
	r, err = m.update(id, func(x *record) error {
		x.State, x.Branch, x.PRURL, x.Note = StateApproved, out.Branch, out.PRURL, out.Note
		return nil
	})
	if err != nil {
		return View{}, err
	}
	m.event(id, lastAgent(r), "approved", "Approve: "+out.Note)
	if !auto {
		go m.kick(r.Project)
	}
	return m.Get(id, 0), nil
}

func lastAgent(r record) string {
	if n := len(r.Agents); n > 0 {
		return r.Agents[n-1]
	}
	return ""
}

func defaultFinalize(now func() time.Time) FinalizeFunc {
	return func(ctx context.Context, project, goal, summary, model string) (Outcome, error) {
		branch := agent.FreeBranch(ctx, project, agent.BranchName(goal, now()))
		if err := agent.Approve(ctx, project, branch, "Edy: "+firstLine(goal)+"\n\n"+summary+"\n\nМодель: "+model); err != nil {
			return Outcome{}, err
		}
		return Outcome{Branch: branch, Note: "один коммит в ветке " + branch}, nil
	}
}

// Reject откатывает проект к состоянию до задачи и запускает следующую.
func (m *Manager) Reject(id string) (View, error) {
	r, err := m.load(id)
	if err != nil {
		return View{}, err
	}
	if r.State != StateDone {
		return View{}, fmt.Errorf("задача в состоянии %q, решение уже не требуется", r.State)
	}
	if err := agent.Reject(context.Background(), r.Project, r.Baseline); err != nil {
		return View{}, err
	}
	r, err = m.update(id, func(x *record) error { x.State = StateRejected; return nil })
	if err != nil {
		return View{}, err
	}
	m.event(id, lastAgent(r), "rejected", "Reject: изменения откачены")
	go m.kick(r.Project)
	return m.Get(id, 0), nil
}

// Cancel останавливает выполняющуюся задачу или снимает её с очереди.
func (m *Manager) Cancel(id string) error {
	r, err := m.load(id)
	if err != nil {
		return err
	}
	switch r.State {
	case StateRunning:
		m.mu.Lock()
		ru := m.active[r.Project]
		m.mu.Unlock()
		if ru == nil || ru.id != id {
			return errors.New("задача уже не выполняется")
		}
		ru.cancel()
		return nil
	case StateQueued:
		if _, err := m.update(id, func(x *record) error { x.State = StateBacklog; return nil }); err != nil {
			return err
		}
		m.event(id, "", "info", "Снята с очереди и возвращена в пул")
		return nil
	}
	return errors.New("задача уже не выполняется")
}

// Comment добавляет комментарий в историю карточки.
func (m *Manager) Comment(id, author, text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("пустой комментарий")
	}
	if _, err := m.load(id); err != nil {
		return err
	}
	m.event(id, "", "comment", author+": "+strings.TrimSpace(text))
	return nil
}

// Edit меняет название и описание задачи в пуле.
func (m *Manager) Edit(id, title, request string) (View, error) {
	_, err := m.update(id, func(x *record) error {
		if x.State != StateBacklog {
			return errors.New("менять можно только задачи в пуле")
		}
		if strings.TrimSpace(request) != "" {
			x.Request = strings.TrimSpace(request)
		}
		x.Title = titleOf(title, x.Request)
		return nil
	})
	if err != nil {
		return View{}, err
	}
	return m.Get(id, 0), nil
}

// Move меняет место карточки в колонке (порядок очереди).
func (m *Manager) Move(id string, position int64) error {
	_, err := m.update(id, func(x *record) error {
		if x.State != StateBacklog && x.State != StateQueued {
			return errors.New("переставлять можно задачи в пуле и в очереди")
		}
		x.Position = position
		return nil
	})
	return err
}

// Delete удаляет карточку (кроме работающей и ждущей решения).
func (m *Manager) Delete(id string) error {
	r, err := m.load(id)
	if err != nil {
		return err
	}
	if r.State == StateRunning || r.State == StateDone {
		return errors.New("сначала остановите задачу или примите решение по ней")
	}
	return m.Board.DeleteBoardTask(id)
}

// Recover вызывается при запуске Edy: задачи, оборванные закрытием
// программы, откатываются и помечаются ошибкой, очереди продолжаются.
func (m *Manager) Recover() {
	list, err := m.Board.BoardTasks("")
	if err != nil {
		return
	}
	projects := map[string]bool{}
	for _, t := range list {
		projects[t.Project] = true
		if t.State != StateRunning {
			continue
		}
		r, err := m.update(t.ID, func(x *record) error {
			x.State, x.Error = StateFailed, "прервана: Edy закрыли во время работы"
			if x.Baseline != "" {
				agent.Reject(context.Background(), x.Project, x.Baseline)
				x.Error += ", проект откачен к началу задачи"
			}
			return nil
		})
		if err == nil {
			m.event(r.ID, "", "error", r.Error)
		}
	}
	for p := range projects {
		m.kick(p)
	}
}

// Wait дожидается завершения всех фоновых задач (для тестов и выключения).
func (m *Manager) Wait() { m.running.Wait() }

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return "task_" + hex.EncodeToString(b)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// titleOf — заголовок карточки: заданный или первая строка просьбы.
func titleOf(title, request string) string {
	t := strings.TrimSpace(title)
	if t == "" {
		t = firstLine(request)
	}
	if r := []rune(t); len(r) > 90 {
		t = string(r[:90]) + "…"
	}
	return t
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
