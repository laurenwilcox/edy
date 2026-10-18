package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/laurenwilcox/name/internal/orchestrator"
	"github.com/laurenwilcox/name/internal/store"
	"github.com/laurenwilcox/name/internal/tasks"
)

// Команда проекта: постоянные агенты с ролью, зоной, моделью и своей памятью.

type teamAgentJSON struct {
	store.TeamAgent
	Done   int `json:"done"`   // принятые задачи и слитые подзадачи
	Failed int `json:"failed"` // не справился или результат отклонили
	Notes  int `json:"notes"`  // заметок в личной памяти
}

func (h *Handler) registerTeamRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/team", h.listTeam)
	m.HandleFunc("POST /api/team", h.saveTeamAgent)
	m.HandleFunc("DELETE /api/team/{id}", h.deleteTeamAgent)
}

// teamStats считает по доске, как агенты справлялись.
func (h *Handler) teamStats(project string) map[string][2]int {
	out := map[string][2]int{}
	if h.Tasks == nil {
		return out
	}
	for _, v := range h.Tasks.List(project) {
		if v.Agent == "" {
			continue
		}
		s := out[v.Agent]
		switch v.State {
		case tasks.StateApproved, tasks.StateMerged:
			s[0]++
		case tasks.StateNeedsHuman, tasks.StateFailed, tasks.StateRejected:
			s[1]++
		}
		out[v.Agent] = s
	}
	return out
}

func (h *Handler) listTeam(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	if !h.knownProject(w, project) {
		return
	}
	team, err := h.Store.Team(project)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	stats := h.teamStats(project)
	notes, _ := h.Store.Notes(project)
	out := []teamAgentJSON{}
	for _, a := range team {
		n := 0
		for _, nt := range notes {
			if nt.Agent == a.ID {
				n++
			}
		}
		out = append(out, teamAgentJSON{TeamAgent: a, Done: stats[a.ID][0], Failed: stats[a.ID][1], Notes: n})
	}
	reply(w, out)
}

func (h *Handler) saveTeamAgent(w http.ResponseWriter, r *http.Request) {
	var a store.TeamAgent
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	if !h.knownProject(w, a.Project) {
		return
	}
	if a.ID == "" {
		b := make([]byte, 6)
		rand.Read(b)
		a.ID = "agent_" + hex.EncodeToString(b)
	} else if old, err := h.Store.TeamAgent(a.ID); err != nil || old.Project != a.Project {
		fail(w, http.StatusNotFound, "агент не найден")
		return
	} else {
		a.Created = old.Created
	}
	a.Name = strings.TrimSpace(a.Name)
	team, _ := h.Store.Team(a.Project)
	for _, o := range team {
		if o.ID != a.ID && strings.EqualFold(o.Name, a.Name) {
			fail(w, http.StatusBadRequest, "в команде уже есть агент «"+o.Name+"»")
			return
		}
	}
	for _, m := range []string{a.Model, a.Fallback} {
		if m == "" {
			continue
		}
		if _, ok := h.checkModels(w, []string{m}); !ok {
			return
		}
	}
	var scope []string
	for _, s := range a.Scope {
		if s = strings.TrimSpace(s); s != "" {
			scope = append(scope, s)
		}
	}
	a.Scope = scope
	if err := h.Store.SaveTeamAgent(a); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	replyStatus(w, http.StatusCreated, a)
}

func (h *Handler) deleteTeamAgent(w http.ResponseWriter, r *http.Request) {
	if err := h.Store.DeleteTeamAgent(r.PathValue("id")); errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "агент не найден")
		return
	} else if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	reply(w, map[string]bool{"ok": true})
}

// teamMembers — команда для инструкций Оркестратора (только включённые).
func (h *Handler) teamMembers(project string) []orchestrator.Member {
	team, _ := h.Store.Team(project)
	stats := h.teamStats(project)
	var out []orchestrator.Member
	for _, a := range team {
		if !a.Enabled {
			continue
		}
		mb := orchestrator.Member{Name: a.Name, Role: a.Role, Scope: a.Scope, Model: a.Model}
		if s := stats[a.ID]; s[0]+s[1] > 0 {
			mb.Stats = fmt.Sprintf("сделал %d задач, не справился с %d", s[0], s[1])
		}
		out = append(out, mb)
	}
	return out
}

// agentByName находит включённого агента команды по имени.
func (h *Handler) agentByName(project, name string) (store.TeamAgent, bool) {
	team, _ := h.Store.Team(project)
	for _, a := range team {
		if a.Enabled && strings.EqualFold(a.Name, name) {
			return a, true
		}
	}
	return store.TeamAgent{}, false
}

// agentModels — модели агента: основная и резервная.
func agentModels(a store.TeamAgent) []string {
	if a.Fallback != "" && a.Fallback != a.Model {
		return []string{a.Model, a.Fallback}
	}
	return []string{a.Model}
}

// union — объединение списков без повторов.
func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string{}, a...), b...) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
