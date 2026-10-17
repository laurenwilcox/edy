// Package tasks запускает задачи агента в фоне и хранит их ленту событий,
// чтобы интерфейс мог показывать ход работы и кнопки Approve/Reject.
package tasks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/laurenwilcox/name/internal/agent"
)

// Состояния задачи.
const (
	StateRunning    = "running"
	StateDone       = "done"        // проверки прошли, ждём решения
	StateNeedsHuman = "needs_human" // ни одна модель не справилась
	StateFailed     = "failed"
	StateCancelled  = "cancelled"
	StateApproved   = "approved"
	StateRejected   = "rejected"
)

var ErrBusy = errors.New("в этом проекте уже идёт задача — дождитесь её или остановите")

// View — снимок задачи для интерфейса.
type View struct {
	ID       string        `json:"id"`
	Project  string        `json:"project"`
	Goal     string        `json:"goal"`
	Models   []string      `json:"models"`
	State    string        `json:"state"`
	Error    string        `json:"error,omitempty"`
	Summary  string        `json:"summary,omitempty"`
	Model    string        `json:"model,omitempty"`
	Diff     string        `json:"diff,omitempty"`
	Branch   string        `json:"branch,omitempty"`
	Created  time.Time     `json:"created"`
	Events   []agent.Event `json:"events"`
	NextFrom int           `json:"next_from"` // индекс, с которого запрашивать новые события
}

type run struct {
	view     View
	baseline string
	cancel   context.CancelFunc
}

// Manager держит задачи в памяти процесса.
type Manager struct {
	LLM agent.LLM
	// Now подменяется в тестах.
	Now func() time.Time

	mu      sync.Mutex
	runs    map[string]*run
	byProj  map[string]string // проект → id выполняющейся задачи
	running sync.WaitGroup
}

func NewManager(llm agent.LLM) *Manager {
	return &Manager{LLM: llm, Now: time.Now, runs: map[string]*run{}, byProj: map[string]string{}}
}

// Start запускает агента в фоне. Проверка «чистого» git делается сразу,
// чтобы ошибка вернулась пользователю, а не потерялась в ленте.
func (m *Manager) Start(project, goal string, models []string) (View, error) {
	if goal == "" || len(models) == 0 {
		return View{}, errors.New("нужны задача и хотя бы одна модель")
	}
	if err := agent.CheckClean(context.Background(), project); err != nil {
		return View{}, err
	}
	m.mu.Lock()
	if id, ok := m.byProj[project]; ok {
		m.mu.Unlock()
		return m.Get(id, 0), ErrBusy
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &run{cancel: cancel, view: View{ID: newID(), Project: project, Goal: goal, Models: models, State: StateRunning, Created: m.Now(), Events: []agent.Event{}}}
	m.runs[r.view.ID] = r
	m.byProj[project] = r.view.ID
	m.mu.Unlock()

	m.running.Add(1)
	go m.work(ctx, r)
	return m.Get(r.view.ID, 0), nil
}

func (m *Manager) work(ctx context.Context, r *run) {
	defer m.running.Done()
	task := &agent.Task{
		Root:   r.view.Project,
		Goal:   r.view.Goal,
		Models: r.view.Models,
		LLM:    m.LLM,
		OnEvent: func(e agent.Event) {
			m.mu.Lock()
			r.view.Events = append(r.view.Events, e)
			m.mu.Unlock()
		},
	}
	res, err := task.Run(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.byProj, r.view.Project)
	r.baseline = res.Baseline
	switch {
	case ctx.Err() != nil:
		r.view.State = StateCancelled
		if r.baseline != "" {
			// Остановленная на полпути работа откатывается целиком.
			if err := agent.Reject(context.Background(), r.view.Project, r.baseline); err != nil {
				r.view.Error = "не удалось откатить: " + err.Error()
			}
		}
		r.view.Events = append(r.view.Events, agent.Event{At: m.Now(), Kind: "info", Text: "Задача остановлена, проект возвращён в исходное состояние"})
	case err != nil:
		r.view.State, r.view.Error = StateFailed, err.Error()
		r.view.Events = append(r.view.Events, agent.Event{At: m.Now(), Kind: "error", Text: err.Error()})
	case res.Status == agent.StatusDone:
		r.view.State, r.view.Summary, r.view.Model, r.view.Diff = StateDone, res.Summary, res.Model, res.Diff
	default:
		r.view.State = StateNeedsHuman
	}
}

// Get возвращает задачу с событиями начиная с индекса from.
func (m *Manager) Get(id string, from int) View {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok {
		return View{}
	}
	v := r.view
	if from < 0 || from > len(v.Events) {
		from = len(v.Events)
	}
	v.Events = append([]agent.Event(nil), v.Events[from:]...)
	v.NextFrom = len(r.view.Events)
	return v
}

// List возвращает задачи без событий, новые первыми.
func (m *Manager) List() []View {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]View, 0, len(m.runs))
	for _, r := range m.runs {
		v := r.view
		v.Events = nil
		v.NextFrom = len(r.view.Events)
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// Running сообщает, идёт ли задача в проекте.
func (m *Manager) Running(project string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.byProj[project]
	return ok
}

// Pending сообщает, ждёт ли в проекте готовый результат решения (Approve/Reject).
func (m *Manager) Pending(project string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.runs {
		if r.view.Project == project && r.view.State == StateDone {
			return true
		}
	}
	return false
}

func (m *Manager) finished(id string) (*run, error) {
	r, ok := m.runs[id]
	if !ok {
		return nil, errors.New("задача не найдена")
	}
	if r.view.State != StateDone {
		return nil, fmt.Errorf("задача в состоянии %q, решение уже не требуется", r.view.State)
	}
	return r, nil
}

// Approve оформляет результат одним коммитом в новой ветке edy/….
func (m *Manager) Approve(id string) (View, error) {
	m.mu.Lock()
	r, err := m.finished(id)
	if err != nil {
		m.mu.Unlock()
		return View{}, err
	}
	branch := agent.BranchName(r.view.Goal, m.Now())
	msg := "Edy: " + firstLine(r.view.Goal) + "\n\n" + r.view.Summary + "\n\nМодель: " + r.view.Model
	project := r.view.Project
	m.mu.Unlock()

	if err := agent.Approve(context.Background(), project, branch, msg); err != nil {
		return View{}, err
	}
	m.mu.Lock()
	r.view.State, r.view.Branch = StateApproved, branch
	r.view.Events = append(r.view.Events, agent.Event{At: m.Now(), Kind: "info", Text: "Approve: один коммит в ветке " + branch})
	m.mu.Unlock()
	return m.Get(id, 0), nil
}

// Reject откатывает проект к состоянию до задачи.
func (m *Manager) Reject(id string) (View, error) {
	m.mu.Lock()
	r, err := m.finished(id)
	if err != nil {
		m.mu.Unlock()
		return View{}, err
	}
	project, baseline := r.view.Project, r.baseline
	m.mu.Unlock()

	if err := agent.Reject(context.Background(), project, baseline); err != nil {
		return View{}, err
	}
	m.mu.Lock()
	r.view.State = StateRejected
	r.view.Events = append(r.view.Events, agent.Event{At: m.Now(), Kind: "info", Text: "Reject: изменения откачены"})
	m.mu.Unlock()
	return m.Get(id, 0), nil
}

// Cancel останавливает выполняющуюся задачу.
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok {
		return errors.New("задача не найдена")
	}
	if r.view.State != StateRunning {
		return errors.New("задача уже не выполняется")
	}
	r.cancel()
	return nil
}

// Wait дожидается завершения всех фоновых задач (для тестов и выключения).
func (m *Manager) Wait() { m.running.Wait() }

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return "task_" + hex.EncodeToString(b)
}

func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' {
			return s[:i]
		}
	}
	return s
}
