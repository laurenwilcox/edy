package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/laurenwilcox/name/internal/agent"
	"github.com/laurenwilcox/name/internal/gitops"
	"github.com/laurenwilcox/name/internal/orchestrator"
	"github.com/laurenwilcox/name/internal/store"
	"github.com/laurenwilcox/name/internal/tasks"
)

const settingProjects = "projects"

func (h *Handler) registerProjectRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/projects", h.listProjects)
	m.HandleFunc("POST /api/projects", h.addProject)
	m.HandleFunc("DELETE /api/projects", h.removeProject)
	m.HandleFunc("GET /api/projects/checkpoints", h.checkpoints)
	m.HandleFunc("POST /api/projects/restore", h.restore)
	m.HandleFunc("POST /api/orchestrator", h.orchestrate)
	m.HandleFunc("GET /api/tasks", h.listTasks)
	m.HandleFunc("POST /api/tasks", h.startTask)
	m.HandleFunc("GET /api/tasks/{id}", h.getTask)
	m.HandleFunc("POST /api/tasks/{id}/{action}", h.taskAction)
}

type projectJSON struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Branch  string `json:"branch,omitempty"`
	Ready   bool   `json:"ready"` // git-репозиторий без незакоммиченных изменений
	Problem string `json:"problem,omitempty"`
	Running bool   `json:"running"`
	Pending bool   `json:"pending"` // готовый результат ждёт Approve/Reject
	Git     bool   `json:"git"`     // git-репозиторий или обычная папка
}

func (h *Handler) projectPaths() []string {
	raw, err := h.Store.Setting(settingProjects)
	if err != nil {
		return nil
	}
	var paths []string
	json.Unmarshal([]byte(raw), &paths)
	return paths
}

func (h *Handler) saveProjectPaths(paths []string) error {
	b, _ := json.Marshal(paths)
	return h.Store.SetSetting(settingProjects, string(b))
}

func (h *Handler) projectInfo(ctx context.Context, path string) projectJSON {
	p := projectJSON{Path: path, Name: filepath.Base(path)}
	if h.Tasks != nil {
		p.Running = h.Tasks.Running(path)
		p.Pending = h.Tasks.Pending(path)
	}
	if err := agent.CheckClean(ctx, path); err != nil {
		p.Problem = err.Error()
	} else {
		p.Ready = true
	}
	p.Git = gitops.IsRepo(path)
	if p.Git {
		p.Branch, _ = agent.Branch(ctx, path)
	}
	return p
}

func (h *Handler) listProjects(w http.ResponseWriter, r *http.Request) {
	out := []projectJSON{}
	for _, path := range h.projectPaths() {
		out = append(out, h.projectInfo(r.Context(), path))
	}
	reply(w, out)
}

// cleanProjectPath проверяет, что путь абсолютный и это существующая папка.
func cleanProjectPath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("укажите полный путь к папке проекта, например /Users/vova/projects/app")
	}
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", errors.New("папка не найдена")
	}
	return path, nil
}

func (h *Handler) addProject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	path, err := cleanProjectPath(req.Path)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	paths := []string{path}
	for _, p := range h.projectPaths() {
		if p != path && len(paths) < 10 {
			paths = append(paths, p)
		}
	}
	if err := h.saveProjectPaths(paths); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	reply(w, h.projectInfo(r.Context(), path))
}

func (h *Handler) removeProject(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	var keep []string
	for _, p := range h.projectPaths() {
		if p != path {
			keep = append(keep, p)
		}
	}
	if err := h.saveProjectPaths(keep); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// knownProject разрешает работать только с папками из списка проектов.
func (h *Handler) knownProject(w http.ResponseWriter, path string) bool {
	for _, p := range h.projectPaths() {
		if p == path {
			return true
		}
	}
	fail(w, http.StatusNotFound, "проект не найден — добавьте его в разделе «Проект»")
	return false
}

func (h *Handler) checkpoints(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if !h.knownProject(w, path) {
		return
	}
	if _, err := os.Stat(filepath.Join(path, ".edy", "shadow.git")); err != nil {
		reply(w, []agent.Checkpoint{})
		return
	}
	cps, err := agent.Shadow{Root: path}.List(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	reply(w, cps)
}

func (h *Handler) restore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
		ID   string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	if !h.knownProject(w, req.Path) {
		return
	}
	if h.Tasks != nil && h.Tasks.Running(req.Path) {
		fail(w, http.StatusConflict, "в проекте идёт задача — сначала остановите её")
		return
	}
	if err := (agent.Shadow{Root: req.Path}).Restore(r.Context(), req.ID); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) orchestrate(w http.ResponseWriter, r *http.Request) {
	if h.LLM == nil {
		fail(w, http.StatusServiceUnavailable, "Оркестратор недоступен")
		return
	}
	var req struct {
		Project      string      `json:"project"`
		Model        string      `json:"model"`
		Messages     []agent.Msg `json:"messages"`
		Conversation string      `json:"conversation"` // id разговора: один разговор — один агент в журнале
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	if !h.knownProject(w, req.Project) {
		return
	}
	// В истории — только реплики пользователя и Оркестратора.
	var history []agent.Msg
	for _, m := range req.Messages {
		if m.Role == "user" || m.Role == "assistant" {
			history = append(history, agent.Msg{Role: m.Role, Content: m.Content})
		}
	}
	agentID := h.journalOrchestrator(req.Conversation, req.Model, req.Project, history)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	out, err := orchestrator.Chat(ctx, h.LLM, req.Model, req.Project, history)
	if agentID != "" {
		switch {
		case err != nil:
			h.Store.AddAgentEvent(agentID, "error", err.Error(), time.Now())
			h.Store.TouchAgent(agentID, store.AgentFailed, err.Error())
		case out.Plan != "":
			h.Store.AddAgentEvent(agentID, "model", out.Text, time.Now())
			h.Store.AddAgentEvent(agentID, "plan", out.Plan, time.Now())
			h.Store.TouchAgent(agentID, store.AgentDone, "Предложен план")
		default:
			h.Store.AddAgentEvent(agentID, "model", out.Text, time.Now())
			h.Store.TouchAgent(agentID, store.AgentDone, "Ждёт ответа разработчика")
		}
	}
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	reply(w, out)
}

// journalOrchestrator ведёт Оркестратора в журнале: агент создаётся на
// первую реплику разговора, дальше в него пишутся реплики и планы.
func (h *Handler) journalOrchestrator(conversation, model, project string, history []agent.Msg) string {
	if conversation == "" || len(history) == 0 || strings.ContainsAny(conversation, " /") || len(conversation) > 64 {
		return ""
	}
	id := "orch-" + conversation
	last := history[len(history)-1]
	if _, err := h.Store.Agent(id); errors.Is(err, store.ErrNotFound) {
		h.Store.CreateAgent(store.Agent{ID: id, Role: store.RoleOrchestrator, Model: model, Project: project, Goal: history[0].Content})
	} else {
		h.Store.TouchAgent(id, store.AgentRunning, "")
	}
	if last.Role == "user" {
		h.Store.AddAgentEvent(id, "user", last.Content, time.Now())
	}
	return id
}

func (h *Handler) listTasks(w http.ResponseWriter, r *http.Request) {
	if h.Tasks == nil {
		reply(w, []tasks.View{})
		return
	}
	reply(w, h.Tasks.List())
}

func (h *Handler) startTask(w http.ResponseWriter, r *http.Request) {
	if h.Tasks == nil {
		fail(w, http.StatusServiceUnavailable, "задачи недоступны")
		return
	}
	var req struct {
		Project      string   `json:"project"`
		Request      string   `json:"request"`
		Plan         string   `json:"plan"`
		Models       []string `json:"models"`
		Conversation string   `json:"conversation"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	if !h.knownProject(w, req.Project) {
		return
	}
	var models []string
	for _, m := range req.Models {
		if p, _, ok := splitModelName(m); ok {
			if _, err := h.Store.Provider(p); err == nil {
				models = append(models, m)
				continue
			}
		}
		fail(w, http.StatusBadRequest, "неизвестная модель "+m)
		return
	}
	v, err := h.Tasks.Start(req.Project, orchestrator.Goal(req.Request, req.Plan), models)
	if errors.Is(err, tasks.ErrBusy) {
		fail(w, http.StatusConflict, err.Error())
		return
	} else if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Conversation != "" {
		if _, err := h.Store.Agent("orch-" + req.Conversation); err == nil {
			h.Store.AddAgentEvent("orch-"+req.Conversation, "info", "План утверждён, запущена задача "+v.ID, time.Now())
		}
	}
	replyStatus(w, http.StatusCreated, v)
}

func (h *Handler) getTask(w http.ResponseWriter, r *http.Request) {
	if h.Tasks == nil {
		fail(w, http.StatusNotFound, "задача не найдена")
		return
	}
	from, _ := strconv.Atoi(r.URL.Query().Get("from"))
	v := h.Tasks.Get(r.PathValue("id"), from)
	if v.ID == "" {
		fail(w, http.StatusNotFound, "задача не найдена")
		return
	}
	reply(w, v)
}

func (h *Handler) taskAction(w http.ResponseWriter, r *http.Request) {
	if h.Tasks == nil {
		fail(w, http.StatusNotFound, "задача не найдена")
		return
	}
	id := r.PathValue("id")
	var v tasks.View
	var err error
	switch r.PathValue("action") {
	case "approve":
		v, err = h.Tasks.Approve(id)
	case "reject":
		v, err = h.Tasks.Reject(id)
	case "cancel":
		err = h.Tasks.Cancel(id)
		v = h.Tasks.Get(id, 0)
	default:
		fail(w, http.StatusNotFound, "неизвестное действие")
		return
	}
	if err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	reply(w, v)
}

func splitModelName(m string) (provider, model string, ok bool) {
	for i := 0; i < len(m); i++ {
		if m[i] == '/' {
			return m[:i], m[i+1:], i > 0 && i < len(m)-1
		}
	}
	return "", "", false
}
