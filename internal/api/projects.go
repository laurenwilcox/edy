package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/laurenwilcox/name/internal/agent"
	"github.com/laurenwilcox/name/internal/codeindex"
	"github.com/laurenwilcox/name/internal/gitops"
	"github.com/laurenwilcox/name/internal/memory"
	"github.com/laurenwilcox/name/internal/omniroute"
	"github.com/laurenwilcox/name/internal/orchestrator"
	"github.com/laurenwilcox/name/internal/skills"
	"github.com/laurenwilcox/name/internal/store"
	"github.com/laurenwilcox/name/internal/tasks"
)

const settingProjects = "projects"

func (h *Handler) registerProjectRoutes(m *routeMux) {
	m.HandleFunc("GET /api/projects", h.listProjects)
	m.HandleFunc("POST /api/projects", h.addProject)
	m.HandleFunc("DELETE /api/projects", h.removeProject)
	m.HandleFunc("GET /api/projects/checkpoints", h.checkpoints)
	m.HandleFunc("POST /api/projects/restore", h.restore)
	m.HandleFunc("GET /api/projects/context", h.projectContext)
	m.HandleFunc("POST /api/orchestrator", h.orchestrate)
	m.HandleFunc("GET /api/tasks", h.listTasks)
	m.HandleFunc("POST /api/tasks", h.startTask)
	m.HandleFunc("POST /api/board", h.createTask)
	m.HandleFunc("POST /api/board/issue", h.importIssue)
	m.HandleFunc("GET /api/templates", h.templates)
	m.HandleFunc("POST /api/templates", h.addTemplate)
	m.HandleFunc("DELETE /api/templates", h.deleteTemplate)
	m.HandleFunc("PUT /api/tasks/{id}", h.editTask)
	m.HandleFunc("DELETE /api/tasks/{id}", h.deleteTask)
	m.HandleFunc("GET /api/tasks/{id}/patch", h.taskPatch)
	m.HandleFunc("POST /api/tasks/{id}/revert-file", h.revertFile)
	m.HandleFunc("POST /api/tasks/{id}/attachments", h.addAttachment)
	m.HandleFunc("GET /api/tasks/{id}/attachments/{file}", h.getAttachment)
	m.HandleFunc("DELETE /api/tasks/{id}/attachments/{file}", h.deleteAttachment)
	m.HandleFunc("GET /api/projects/files", h.projectFiles)
	m.HandleFunc("GET /api/stats/models", h.modelStats)
	m.HandleFunc("GET /api/projects/file", h.projectFile)
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

// projectContext показывает то, что видят агенты: скелет проекта или результаты поиска.
func (h *Handler) projectContext(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if !h.knownProject(w, path) {
		return
	}
	idx, err := codeindex.Build(r.Context(), path)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		reply(w, map[string]string{"text": codeindex.FormatHits(idx.Search(q, 8), 40)})
		return
	}
	reply(w, map[string]string{"text": idx.Skeleton(60000)})
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
		Executors    []string    `json:"executors"`    // выбранные исполнитель и резерв
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
	oc := h.orchestratorContext(req.Project, req.Executors)
	oc.Git = gitops.IsRepo(req.Project)
	oc.Team = h.teamMembers(req.Project)
	if ps, err := h.Store.ProjectSettings(req.Project); err == nil {
		oc.Parallel = ps.MaxParallel
	}
	if notes, err := h.Store.Notes(req.Project); err == nil && len(history) > 0 {
		// Выжимка под последнюю просьбу: не вся память, а похожее (≈500 токенов).
		oc.Memory = memory.Brief(notes, history[len(history)-1].Content, 2000)
	}
	out, err := orchestrator.Chat(ctx, h.LLM, req.Model, req.Project, history, oc)
	if err == nil && out.Agent != "" && agentID != "" {
		h.Store.AddAgentEvent(agentID, "info", "Отдаёт задачу агенту команды «"+out.Agent+"»", time.Now())
	}
	if agentID != "" {
		switch {
		case err != nil:
			h.Store.AddAgentEvent(agentID, "error", err.Error(), time.Now())
			h.Store.TouchAgent(agentID, store.AgentFailed, err.Error())
		case out.Plan != "":
			h.Store.AddAgentEvent(agentID, "model", out.Text, time.Now())
			h.Store.AddAgentEvent(agentID, "plan", out.Plan, time.Now())
			if len(out.MCP) > 0 {
				h.Store.AddAgentEvent(agentID, "info", "Выдаёт исполнителю MCP: "+strings.Join(out.MCP, ", "), time.Now())
			}
			if len(out.Skills) > 0 {
				h.Store.AddAgentEvent(agentID, "info", "Выдаёт исполнителю скиллы: "+strings.Join(out.Skills, ", "), time.Now())
			}
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

// orchestratorContext — что Оркестратор знает кроме проекта: возможности
// исполнителей и других моделей, скиллы проекта и каталог MCP-серверов.
func (h *Handler) orchestratorContext(project string, executors []string) orchestrator.Context {
	var c orchestrator.Context
	providers, _ := h.Store.Providers()
	byName := map[string]store.Provider{}
	for _, p := range providers {
		byName[p.Name] = p
	}
	statByModel := map[string]string{}
	if stats, err := h.Store.ModelStats(time.Now().AddDate(0, 0, -30)); err == nil {
		for _, st := range stats {
			statByModel[st.Model] = statLine(st)
		}
	}
	describe := func(model string) (orchestrator.Executor, bool) {
		name, _, ok := splitModelName(model)
		p, found := byName[name]
		if !ok || !found {
			return orchestrator.Executor{}, false
		}
		e := orchestrator.Executor{Model: model, MCP: omniroute.HasCap(p, omniroute.CapMCP), Stats: statByModel[model]}
		for _, cp := range omniroute.Caps(p) {
			e.Caps = append(e.Caps, omniroute.CapTitle(cp))
		}
		return e, true
	}
	chosen := map[string]bool{}
	for _, m := range executors {
		if e, ok := describe(m); ok && !chosen[m] {
			chosen[m] = true
			c.Executors = append(c.Executors, e)
		}
	}
	for _, p := range providers {
		for _, m := range p.Models {
			full := p.Name + "/" + m
			if e, ok := describe(full); ok && !chosen[full] && len(c.Others) < 30 {
				c.Others = append(c.Others, e)
			}
		}
	}
	if list, err := skills.Load(skills.Dirs(h.Home, project)); err == nil {
		for _, sk := range list {
			c.Skills = append(c.Skills, orchestrator.Skill{Name: sk.Name, Description: sk.Description})
		}
	}
	c.Servers = h.orchestratorServers()
	return c
}

// statLine — статистика модели одной строкой для Оркестратора.
func statLine(st store.ModelStat) string {
	if st.Attempts == 0 {
		return ""
	}
	line := fmt.Sprintf("за 30 дней прошла проверки в %d из %d задач", st.Successes, st.Attempts)
	switch {
	case st.CostUSD > 0 && st.Successes > 0:
		line += fmt.Sprintf(", ≈$%.2f за успешную задачу", st.CostUSD/float64(st.Successes))
	case st.CostUSD == 0:
		line += ", без оплаты за токены (подписка или локальная)"
	}
	return line
}

// modelStats — статистика моделей для экрана «Расходы».
func (h *Handler) modelStats(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 365 {
		days = 30
	}
	stats, err := h.Store.ModelStats(time.Now().AddDate(0, 0, -days))
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	reply(w, stats)
}

// orchestratorServers — включённые серверы каталога MCP для инструкций Оркестратора.
func (h *Handler) orchestratorServers() []orchestrator.Server {
	list, err := h.Store.MCPServers()
	if err != nil {
		return nil
	}
	var out []orchestrator.Server
	for _, m := range list {
		if m.Enabled {
			desc := m.Description
			if desc == "" {
				desc = "без описания"
			}
			out = append(out, orchestrator.Server{Name: m.Name, Description: desc, Account: m.Account})
		}
	}
	return out
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
	project := r.URL.Query().Get("project")
	if project != "" && !h.knownProject(w, project) {
		return
	}
	reply(w, h.Tasks.List(project))
}

// checkModels проверяет, что модели «провайдер/модель» подключены.
func (h *Handler) checkModels(w http.ResponseWriter, list []string) ([]string, bool) {
	var models []string
	for _, m := range list {
		if p, _, ok := splitModelName(m); ok {
			if _, err := h.Store.Provider(p); err == nil {
				models = append(models, m)
				continue
			}
		}
		fail(w, http.StatusBadRequest, "неизвестная модель "+m)
		return nil, false
	}
	return models, true
}

// startTask ставит задачу в очередь проекта с утверждённым планом: новую
// или из пула (id). Если проект свободен, работа начинается сразу.
func (h *Handler) startTask(w http.ResponseWriter, r *http.Request) {
	if h.Tasks == nil {
		fail(w, http.StatusServiceUnavailable, "задачи недоступны")
		return
	}
	var req struct {
		ID           string   `json:"id"` // задача из пула
		Project      string   `json:"project"`
		Parent       string   `json:"parent"`
		Title        string   `json:"title"`
		Request      string   `json:"request"`
		Plan         string   `json:"plan"`
		Comment      string   `json:"comment"` // пояснение Оркестратора к плану
		Models       []string `json:"models"`
		Conversation string   `json:"conversation"`
		// Subtasks — задача разбита Оркестратором на части для нескольких агентов.
		Subtasks []orchestrator.Subtask `json:"subtasks"`
		// Agent — постоянный агент команды (id), выбранный исполнителем вручную.
		Agent string `json:"agent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	if !h.knownProject(w, req.Project) {
		return
	}
	models, ok := h.checkModels(w, req.Models)
	if !ok {
		return
	}
	// Скиллы и внешние MCP-серверы берутся из утверждённого плана: их выдал
	// Оркестратор. Без плана (задача поставлена напрямую) — все скиллы.
	var opts tasks.Options
	if strings.TrimSpace(req.Plan) != "" {
		oc := h.orchestratorContext(req.Project, models)
		opts.MCP = orchestrator.PlanMCP(req.Plan, oc.Servers)
		opts.Skills = orchestrator.PlanSkills(req.Plan, oc.Skills)
		if opts.Skills == nil {
			opts.Skills = []string{}
		}
	}
	if req.ID != "" {
		if cur := h.Tasks.Get(req.ID, 0); cur.ID == "" || cur.Project != req.Project {
			fail(w, http.StatusNotFound, "задача не найдена")
			return
		}
	} else if strings.TrimSpace(req.Plan) == "" {
		// Без плана задача сразу идёт в работу: проект должен быть готов.
		if err := agent.CheckClean(r.Context(), req.Project); err != nil && !h.Tasks.Running(req.Project) {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	// Агент команды: выбран вручную или назначен Оркестратором в плане.
	var teamAgent *store.TeamAgent
	if req.Agent != "" {
		a, err := h.Store.TeamAgent(req.Agent)
		if err != nil || a.Project != req.Project {
			fail(w, http.StatusBadRequest, "агент команды не найден")
			return
		}
		teamAgent = &a
	} else if len(req.Subtasks) < 2 {
		if name := orchestrator.PlanAgent(req.Plan, h.teamMembers(req.Project)); name != "" {
			if a, ok := h.agentByName(req.Project, name); ok {
				teamAgent = &a
			}
		}
	}
	if teamAgent != nil {
		models = agentModels(*teamAgent)
		opts.MCP = union(opts.MCP, teamAgent.MCP)
		if opts.Skills != nil {
			opts.Skills = union(opts.Skills, teamAgent.Skills)
		}
	}
	draft := tasks.Draft{Project: req.Project, Parent: req.Parent, Title: req.Title, Request: req.Request}
	plan := tasks.Plan{Request: req.Request, Plan: req.Plan, Goal: orchestrator.Goal(req.Request, req.Plan), Models: models, Options: opts}
	if teamAgent != nil {
		plan.Agent = teamAgent.ID
	}
	var v tasks.View
	var err error
	if req.ID != "" && strings.TrimSpace(req.Plan) != "" && len(req.Subtasks) < 2 {
		// Новый план без деления: прежние подзадачи эпика больше не нужны.
		if err := h.Tasks.Unsplit(req.ID); err != nil {
			fail(w, http.StatusConflict, err.Error())
			return
		}
	}
	if len(req.Subtasks) >= 2 {
		// Несколько агентов: у каждой подзадачи свой исполнитель, резерв —
		// резервная модель задачи.
		var subs []tasks.SubPlan
		for _, st := range req.Subtasks {
			if a, ok := h.agentByName(req.Project, st.Agent); st.Agent != "" && ok {
				// Подзадачу ведёт агент команды — со своей моделью и памятью.
				subs = append(subs, tasks.SubPlan{Title: st.Title, Goal: st.Goal, Models: agentModels(a), After: st.After, Agent: a.ID})
				continue
			}
			sm, ok := h.checkModels(w, []string{st.Model})
			if !ok {
				return
			}
			if len(models) > 1 && models[1] != sm[0] {
				sm = append(sm, models[1])
			}
			subs = append(subs, tasks.SubPlan{Title: st.Title, Goal: st.Goal, Models: sm, After: st.After})
		}
		v, err = h.Tasks.EnqueueEpic(req.ID, draft, plan, subs)
	} else {
		v, err = h.Tasks.Enqueue(req.ID, draft, plan)
	}
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Comment) != "" {
		h.Tasks.Comment(v.ID, "Оркестратор", req.Comment)
	}
	if req.Conversation != "" {
		if _, err := h.Store.Agent("orch-" + req.Conversation); err == nil {
			h.Store.AddAgentEvent("orch-"+req.Conversation, "info", "План утверждён, задача "+v.ID+" поставлена в очередь", time.Now())
		}
	}
	replyStatus(w, http.StatusCreated, h.Tasks.Get(v.ID, 0))
}

// createTask кладёт задачу в пул доски (без запуска).
func (h *Handler) createTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Project string `json:"project"`
		Parent  string `json:"parent"`
		Title   string `json:"title"`
		Request string `json:"request"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	if h.Tasks == nil || !h.knownProject(w, req.Project) {
		return
	}
	v, err := h.Tasks.Create(tasks.Draft{Project: req.Project, Parent: req.Parent, Title: req.Title, Request: req.Request})
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	replyStatus(w, http.StatusCreated, v)
}

// importIssue кладёт в пул задачу из issue GitHub, GitLab или Gitea.
func (h *Handler) importIssue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Project string `json:"project"`
		URL     string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	if h.Tasks == nil || !h.knownProject(w, req.Project) {
		return
	}
	accounts, _ := h.Store.GitAccounts()
	ref, err := gitops.ParseIssueURL(req.URL, accounts)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	var acc *store.GitAccount
	for _, a := range accounts {
		if strings.TrimRight(a.Host, "/") == ref.Host {
			if full, err := h.Store.GitAccount(a.ID); err == nil {
				acc = &full
				break
			}
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	is, err := gitops.FetchIssue(ctx, ref, acc, nil)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	body := strings.TrimSpace(is.Body)
	if rs := []rune(body); len(rs) > 6000 {
		body = string(rs[:6000]) + "\n…(текст issue обрезан)"
	}
	request := "Issue: " + is.URL
	if len(is.Labels) > 0 {
		request += "\nМетки: " + strings.Join(is.Labels, ", ")
	}
	request += "\n\n" + is.Title + "\n\n" + body
	closing := fmt.Sprintf("#%d", is.Number)
	if ref.Kind == store.GitHub {
		closing = fmt.Sprintf("%s#%d", ref.Repo, is.Number)
	}
	v, err := h.Tasks.Create(tasks.Draft{Project: req.Project, Title: fmt.Sprintf("#%d %s", is.Number, is.Title), Request: request, IssueURL: is.URL, IssueRef: closing})
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	replyStatus(w, http.StatusCreated, v)
}

// Шаблоны частых задач: встроенные и свои.
var builtinTemplates = []string{
	"Добавь тесты к {файл}: покрой основные сценарии и крайние случаи",
	"Найди и исправь падающий тест",
	"Обнови зависимости до свежих версий и убедись, что тесты проходят",
	"Отрефактори {файл}: упрости код, не меняя поведения",
	"Добавь обработку ошибок в {файл}",
	"Найди и удали мёртвый код",
	"Проверь проект на секреты в коде и небезопасные места",
	"Напиши README по текущему коду: что это, как собрать, как запустить тесты",
}

const settingTemplates = "task_templates"

func (h *Handler) customTemplates() []string {
	raw, _ := h.Store.Setting(settingTemplates)
	var list []string
	json.Unmarshal([]byte(raw), &list)
	return list
}

func (h *Handler) templates(w http.ResponseWriter, r *http.Request) {
	custom := h.customTemplates()
	if custom == nil {
		custom = []string{}
	}
	reply(w, map[string][]string{"builtin": builtinTemplates, "custom": custom})
}

func (h *Handler) addTemplate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
		fail(w, http.StatusBadRequest, "пустой шаблон")
		return
	}
	text := strings.TrimSpace(req.Text)
	if len([]rune(text)) > 1000 {
		fail(w, http.StatusBadRequest, "шаблон длиннее 1000 символов")
		return
	}
	list := h.customTemplates()
	for _, t := range list {
		if t == text {
			h.templates(w, r)
			return
		}
	}
	if len(list) >= 50 {
		fail(w, http.StatusBadRequest, "не больше 50 своих шаблонов")
		return
	}
	b, _ := json.Marshal(append(list, text))
	if err := h.Store.SetSetting(settingTemplates, string(b)); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.templates(w, r)
}

func (h *Handler) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	text := r.URL.Query().Get("text")
	var keep []string
	for _, t := range h.customTemplates() {
		if t != text {
			keep = append(keep, t)
		}
	}
	b, _ := json.Marshal(keep)
	h.Store.SetSetting(settingTemplates, string(b))
	h.templates(w, r)
}

func (h *Handler) getTask(w http.ResponseWriter, r *http.Request) {
	if h.Tasks == nil {
		fail(w, http.StatusNotFound, "задача не найдена")
		return
	}
	from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	v := h.Tasks.Get(r.PathValue("id"), from)
	if v.ID == "" {
		fail(w, http.StatusNotFound, "задача не найдена")
		return
	}
	reply(w, v)
}

func (h *Handler) editTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title    *string `json:"title"`
		Request  *string `json:"request"`
		Position *int64  `json:"position"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	id := r.PathValue("id")
	if h.Tasks == nil {
		fail(w, http.StatusNotFound, "задача не найдена")
		return
	}
	if req.Position != nil {
		if err := h.Tasks.Move(id, *req.Position); err != nil {
			fail(w, http.StatusConflict, err.Error())
			return
		}
	}
	if req.Title != nil || req.Request != nil {
		title, request := "", ""
		if req.Title != nil {
			title = *req.Title
		}
		if req.Request != nil {
			request = *req.Request
		}
		if _, err := h.Tasks.Edit(id, title, request); err != nil {
			fail(w, http.StatusConflict, err.Error())
			return
		}
	}
	reply(w, h.Tasks.Get(id, 0))
}

func (h *Handler) deleteTask(w http.ResponseWriter, r *http.Request) {
	if h.Tasks == nil {
		fail(w, http.StatusNotFound, "задача не найдена")
		return
	}
	if err := h.Tasks.Delete(r.PathValue("id")); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	reply(w, map[string]bool{"ok": true})
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
	case "comment":
		var req struct {
			Text string `json:"text"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		err = h.Tasks.Comment(id, "Вы", req.Text)
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

func (h *Handler) addAttachment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Data string `json:"data"` // data:image/png;base64,…
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON или картинка больше 5 МБ")
		return
	}
	if h.Tasks == nil {
		fail(w, http.StatusNotFound, "задача не найдена")
		return
	}
	v, err := h.Tasks.Attach(r.PathValue("id"), req.Name, req.Data)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	replyStatus(w, http.StatusCreated, v)
}

func (h *Handler) getAttachment(w http.ResponseWriter, r *http.Request) {
	if h.Tasks == nil {
		fail(w, http.StatusNotFound, "задача не найдена")
		return
	}
	path, err := h.Tasks.AttachmentPath(r.PathValue("id"), r.PathValue("file"))
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeFile(w, r, path)
}

func (h *Handler) deleteAttachment(w http.ResponseWriter, r *http.Request) {
	if h.Tasks == nil {
		fail(w, http.StatusNotFound, "задача не найдена")
		return
	}
	v, err := h.Tasks.RemoveAttachment(r.PathValue("id"), r.PathValue("file"))
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	reply(w, v)
}

func (h *Handler) taskPatch(w http.ResponseWriter, r *http.Request) {
	if h.Tasks == nil {
		fail(w, http.StatusNotFound, "задача не найдена")
		return
	}
	patch, err := h.Tasks.Patch(r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	reply(w, map[string]string{"patch": patch})
}

func (h *Handler) revertFile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		File string `json:"file"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.File == "" {
		fail(w, http.StatusBadRequest, "укажите файл")
		return
	}
	if h.Tasks == nil {
		fail(w, http.StatusNotFound, "задача не найдена")
		return
	}
	v, err := h.Tasks.RevertFile(r.PathValue("id"), req.File)
	if err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	reply(w, v)
}

// projectFiles — список файлов проекта для просмотра.
func (h *Handler) projectFiles(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if !h.knownProject(w, path) {
		return
	}
	files, err := agent.Workspace{Root: path}.Files(5000)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if files == nil {
		files = []string{}
	}
	reply(w, map[string]any{"files": files, "truncated": len(files) >= 5000})
}

// projectFile — текст файла проекта (только внутри проекта, не больше 1 МБ).
func (h *Handler) projectFile(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	path := q.Get("path")
	if !h.knownProject(w, path) {
		return
	}
	text, truncated, err := agent.Workspace{Root: path}.ReadText(q.Get("file"), 1<<20)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	reply(w, map[string]any{"file": q.Get("file"), "text": text, "truncated": truncated})
}

func splitModelName(m string) (provider, model string, ok bool) {
	for i := 0; i < len(m); i++ {
		if m[i] == '/' {
			return m[:i], m[i+1:], i > 0 && i < len(m)-1
		}
	}
	return "", "", false
}
