package tasks

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/laurenwilcox/name/internal/agent"
	"github.com/laurenwilcox/name/internal/skills"
)

// Эпик — большая задача, которую Оркестратор разбил на подзадачи для
// нескольких агентов. Независимые подзадачи идут одновременно, каждая в своей
// копии проекта (git worktree); готовая подзадача сливается в общую ветку
// эпика, и следующие начинают уже с её изменениями. Когда всё слито, Edy
// прогоняет проверки и ревью на общем результате, а Approve даётся один раз
// на весь эпик.

const (
	KindEpic = "epic"
	KindSub  = "sub"

	maxSubtasks = 6
)

// SubPlan — подзадача из плана Оркестратора.
type SubPlan struct {
	Title  string
	Goal   string
	Models []string // исполнитель подзадачи (и резерв)
	Agent  string   // постоянный агент, если подзадачу ведёт он
	After  []int    // номера подзадач (с 1), которые должны быть готовы раньше
}

// Unsplit убирает у задачи прежнее деление на подзадачи — перед новым
// планом Оркестратора (он может разделить иначе или не делить вовсе).
func (m *Manager) Unsplit(id string) error {
	r, err := m.load(id)
	if err != nil || r.Kind != KindEpic {
		return err
	}
	switch r.State {
	case StateRunning, StateDone, StateQueued:
		return errors.New("эпик сейчас в работе или ждёт решения")
	}
	m.cleanupEpic(r)
	for _, c := range m.children(id, r.Project) {
		m.Board.DeleteBoardTask(c.ID)
	}
	_, err = m.update(id, func(x *record) error { x.Kind, x.EpicBranch, x.EpicBase = "", "", ""; return nil })
	return err
}

// EnqueueEpic ставит в очередь задачу, разбитую на подзадачи.
func (m *Manager) EnqueueEpic(id string, d Draft, p Plan, subs []SubPlan) (View, error) {
	if len(subs) < 2 || len(subs) > maxSubtasks {
		return View{}, fmt.Errorf("в эпике от 2 до %d подзадач", maxSubtasks)
	}
	for i, sp := range subs {
		if strings.TrimSpace(sp.Goal) == "" || len(sp.Models) == 0 {
			return View{}, fmt.Errorf("подзадача %d: нужны описание и модель", i+1)
		}
		for _, a := range sp.After {
			// Ссылки только на более ранние подзадачи — циклов быть не может.
			if a < 1 || a > i {
				return View{}, fmt.Errorf("подзадача %d: может ждать только подзадачи с меньшими номерами", i+1)
			}
		}
	}
	if !isGitRepo(firstNonEmpty(d.Project, m.projectOf(id))) {
		return View{}, errors.New("несколько агентов работают в копиях проекта, это требует git — выполните в папке git init")
	}
	if len(p.Models) == 0 {
		p.Models = subs[0].Models
	}
	if id != "" {
		if err := m.Unsplit(id); err != nil {
			return View{}, err
		}
	}
	// Эпик ставится в очередь без запуска: стартовать он должен, когда
	// подзадачи уже созданы.
	v, err := m.enqueue(id, d, p, false)
	if err != nil {
		return View{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ep, err := m.load(v.ID)
	if err != nil {
		return View{}, err
	}
	titles := make([]string, len(subs))
	for i, sp := range subs {
		titles[i] = fmt.Sprintf("%d. %s", i+1, sp.Title)
	}
	for i, sp := range subs {
		sub := record{View: View{ID: newID(), Project: ep.Project, Parent: ep.ID, Kind: KindSub, Agent: sp.Agent, Step: i + 1, After: sp.After,
			Title: titleOf(sp.Title, sp.Goal), Request: sp.Goal, Models: sp.Models, MCP: ep.MCP, Skills: ep.Skills,
			State: StateQueued, Created: m.Now(), Position: m.Now().UnixMilli() + int64(i), Agents: []string{}},
			AllSkills: ep.AllSkills}
		sub.Goal = fmt.Sprintf("Общая задача:\n%s\n\nТвоя часть — подзадача %d из %d: %s\n%s\n\nВсе части эпика:\n%s\n\n"+
			"Остальные части делают другие агенты параллельно, в своих копиях проекта. Делай только свою часть и не трогай файлы, которые относятся к другим частям, без необходимости.",
			ep.Goal, i+1, len(subs), sp.Title, sp.Goal, strings.Join(titles, "\n"))
		if err := m.save(&sub); err != nil {
			return View{}, err
		}
		m.Board.AddBoardEvent(sub.ID, "created", "Подзадача эпика «"+ep.Title+"», исполнитель "+strings.Join(sp.Models, " → "), m.Now())
	}
	ep.Kind = KindEpic
	if err := m.save(&ep); err != nil {
		return View{}, err
	}
	m.Board.AddBoardEvent(ep.ID, "subtask", fmt.Sprintf("Оркестратор разбил задачу на %d подзадач:\n%s", len(subs), strings.Join(titles, "\n")), m.Now())
	go m.kick(ep.Project)
	return m.Get(ep.ID, 0), nil
}

func (m *Manager) projectOf(id string) string {
	if id == "" {
		return ""
	}
	r, err := m.load(id)
	if err != nil {
		return ""
	}
	return r.Project
}

// children — подзадачи эпика по порядку.
func (m *Manager) children(epicID string, project string) []record {
	list, _ := m.Board.BoardTasks(project)
	var out []record
	for _, t := range list {
		if t.Parent != epicID {
			continue
		}
		if r, err := m.load(t.ID); err == nil && r.Kind == KindSub {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Step < out[j].Step })
	return out
}

func (m *Manager) maxParallel(project string) int {
	if m.MaxParallel != nil {
		if n := m.MaxParallel(project); n > 0 {
			return n
		}
	}
	return 2
}

type subResult struct {
	step int
	ok   bool
}

// runEpic ведёт эпик: раздаёт подзадачи агентам, сливает результаты,
// проверяет общий итог.
func (m *Manager) runEpic(ctx context.Context, ru *run, ep record) {
	defer m.running.Done()
	defer func() {
		m.mu.Lock()
		delete(m.active, ep.Project)
		m.mu.Unlock()
		m.kick(ep.Project)
	}()
	bg := context.Background()
	par := m.maxParallel(ep.Project)
	subs := m.children(ep.ID, ep.Project)
	m.event(ep.ID, "", "started", fmt.Sprintf("Эпик начат: %d подзадач, одновременно до %d агентов, каждый в своей копии проекта", len(subs), par))

	fail := func(msg string) {
		r, _ := m.update(ep.ID, func(x *record) error { x.State, x.Error = StateNeedsHuman, msg; return nil })
		m.event(ep.ID, "", "error", msg)
		m.cleanupEpic(r)
		m.notify(r.View, "Нужна помощь: "+msg)
	}
	base, err := gitC(bg, ep.Project, "rev-parse", "HEAD")
	if err != nil {
		fail("не удалось прочитать текущий коммит: " + err.Error())
		return
	}
	branch := "edy/epic-" + strings.TrimPrefix(ep.ID, "task_")
	integ := filepath.Join(epicDir(ep.Project, ep.ID), "main")
	removeEpicWorktrees(ep.Project, ep.ID, []string{branch})
	if err := addWorktree(bg, ep.Project, integ, branch, base); err != nil {
		fail("не удалось создать общую копию проекта: " + err.Error())
		return
	}
	ep, _ = m.update(ep.ID, func(x *record) error { x.EpicBase, x.EpicBranch = base, branch; return nil })

	var integMu sync.Mutex
	results := make(chan subResult)
	merged := map[int]bool{}
	started := map[int]bool{}
	running, failed := 0, false
	for {
		if !failed && ctx.Err() == nil {
			for _, s := range subs {
				if running >= par || started[s.Step] {
					continue
				}
				ready := true
				for _, a := range s.After {
					ready = ready && merged[a]
				}
				if !ready {
					continue
				}
				started[s.Step] = true
				running++
				m.running.Add(1)
				go func(s record) {
					defer m.running.Done()
					results <- subResult{s.Step, m.runSub(ctx, ep, s, integ, &integMu)}
				}(s)
			}
		}
		if running == 0 {
			break
		}
		res := <-results
		running--
		if res.ok {
			merged[res.step] = true
		} else {
			failed = true
		}
	}

	switch {
	case ctx.Err() != nil:
		r, _ := m.update(ep.ID, func(x *record) error { x.State = StateCancelled; return nil })
		m.cleanupEpic(r)
		m.event(ep.ID, "", "info", "Эпик остановлен, копии проекта удалены; основная папка не менялась")
		return
	case failed || len(merged) < len(subs):
		fail("не все подзадачи готовы — посмотрите подзадачи с ошибкой и запустите эпик ещё раз")
		return
	}

	// Общий результат: проверки на слитой копии.
	hooks, err := agent.DetectHooks(integ)
	if err != nil {
		fail(err.Error())
		return
	}
	if len(hooks) > 0 {
		ok, report := agent.RunHooks(bg, integ, hooks)
		m.event(ep.ID, "", "hooks", "Проверки общего результата:\n"+strings.TrimSpace(report))
		if !ok {
			fail("подзадачи слиты, но общие проверки не прошли — части не сошлись друг с другом")
			return
		}
	}
	stat, _ := gitC(bg, integ, "diff", "--stat", base)
	patch, _ := gitC(bg, integ, "diff", "--no-color", "-U3", base)
	if len(patch) > patchLimit {
		patch = patch[:patchLimit] + "\n… (изменения обрезаны)"
	}
	var summaries []string
	for _, s := range m.children(ep.ID, ep.Project) {
		summaries = append(summaries, fmt.Sprintf("%d. %s — %s", s.Step, s.Title, firstNonEmpty(s.Summary, "готово")))
	}
	final, _ := m.update(ep.ID, func(x *record) error {
		x.State, x.Diff, x.Patch = StateDone, stat, patch
		x.Summary = strings.Join(summaries, "\n")
		x.Model = strings.Join(x.Models, ", ")
		return nil
	})
	m.event(ep.ID, "", "info", "Все подзадачи слиты, общие проверки прошли. Проверьте изменения и нажмите Approve.")
	m.afterDone(final, "")
}

// runSub выполняет одну подзадачу в своей копии проекта и сливает её.
func (m *Manager) runSub(ctx context.Context, ep, sub record, integ string, integMu *sync.Mutex) bool {
	bg := context.Background()
	failSub := func(state, msg string) bool {
		m.update(sub.ID, func(x *record) error { x.State, x.Error = state, msg; return nil })
		m.event(sub.ID, "", "error", msg)
		m.event(ep.ID, "", "error", fmt.Sprintf("Подзадача %d «%s»: %s", sub.Step, sub.Title, msg))
		return false
	}
	integMu.Lock()
	head, err := gitC(bg, integ, "rev-parse", "HEAD")
	integMu.Unlock()
	if err != nil {
		return failSub(StateFailed, err.Error())
	}
	wt := filepath.Join(epicDir(ep.Project, ep.ID), fmt.Sprint(sub.Step))
	br := fmt.Sprintf("%s-%d", ep.EpicBranch, sub.Step)
	if err := addWorktree(bg, ep.Project, wt, br, head); err != nil {
		return failSub(StateFailed, "не удалось создать копию проекта: "+err.Error())
	}
	defer gitC(bg, ep.Project, "worktree", "remove", "--force", wt)
	sub, _ = m.update(sub.ID, func(x *record) error { x.State, x.SubBranch = StateRunning, br; return nil })
	m.event(sub.ID, "", "started", "Работа началась в своей копии проекта")
	m.event(ep.ID, "", "model", fmt.Sprintf("Подзадача %d «%s» → %s", sub.Step, sub.Title, sub.Models[0]))

	given := m.skillsFor(sub)
	res, err, agentID := m.execute(ctx, &run{id: sub.ID}, sub, wt, sub.Goal, given)
	switch {
	case ctx.Err() != nil:
		m.update(sub.ID, func(x *record) error { x.State = StateCancelled; return nil })
		return false
	case err != nil:
		return failSub(StateFailed, err.Error())
	case res.Status != agent.StatusDone:
		return failSub(StateNeedsHuman, "ни одна модель не прошла проверки")
	}
	if _, err := commitAll(bg, wt, fmt.Sprintf("Подзадача %d: %s", sub.Step, sub.Title)); err != nil {
		return failSub(StateFailed, "не удалось зафиксировать результат: "+err.Error())
	}
	integMu.Lock()
	_, err = gitC(bg, integ, "merge", "--no-ff", "--no-edit", "-m", fmt.Sprintf("Эпик: подзадача %d «%s»", sub.Step, sub.Title), br)
	if err != nil {
		gitC(bg, integ, "merge", "--abort")
	}
	integMu.Unlock()
	if err != nil {
		return failSub(StateNeedsHuman, "конфликт при слиянии с другими подзадачами — части правят одно и то же место")
	}
	m.update(sub.ID, func(x *record) error {
		x.State, x.Summary, x.Model, x.Diff = StateMerged, res.Summary, res.Model, res.Diff
		return nil
	})
	m.event(sub.ID, agentID, "approved", "Готово и слито в общую ветку эпика")
	m.event(ep.ID, "", "info", fmt.Sprintf("Подзадача %d «%s» готова (%s) и слита", sub.Step, sub.Title, res.Model))
	return true
}

// skillsFor — скиллы, выданные задаче.
func (m *Manager) skillsFor(r record) []skills.Skill {
	list, _ := skills.Load(skills.Dirs(m.SkillsHome, r.Project))
	if r.AllSkills {
		return list
	}
	given := []skills.Skill{}
	for _, name := range r.Skills {
		if sk, ok := skills.Find(list, name); ok {
			given = append(given, sk)
		}
	}
	return given
}

// cleanupEpic удаляет копии проекта и ветки эпика.
func (m *Manager) cleanupEpic(ep record) {
	branches := []string{ep.EpicBranch}
	for _, s := range m.children(ep.ID, ep.Project) {
		branches = append(branches, s.SubBranch)
	}
	removeEpicWorktrees(ep.Project, ep.ID, branches)
}

// applyEpic переносит результат эпика в рабочую папку проекта (без
// коммита), чтобы дальше его оформили настройки Git проекта.
func (m *Manager) applyEpic(ep record) error {
	ctx := context.Background()
	if err := agent.CheckClean(ctx, ep.Project); err != nil {
		return err
	}
	if _, err := gitC(ctx, ep.Project, "merge", "--squash", ep.EpicBranch); err != nil {
		// Проект был чистым — возвращаем его как было.
		gitC(ctx, ep.Project, "reset", "-q", "--hard", "HEAD")
		return fmt.Errorf("результат эпика не накладывается на текущее состояние проекта (его меняли после начала эпика): %w", err)
	}
	return nil
}

// resetEpic готовит эпик к повторному запуску: подзадачи снова в очереди.
func (m *Manager) resetEpic(ep record) {
	for _, s := range m.children(ep.ID, ep.Project) {
		m.update(s.ID, func(x *record) error {
			x.State, x.Error, x.Summary, x.Diff, x.SubBranch = StateQueued, "", "", "", ""
			return nil
		})
	}
}
