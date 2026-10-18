package api

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/laurenwilcox/name/internal/mcp"
	"github.com/laurenwilcox/name/internal/skills"
	"github.com/laurenwilcox/name/internal/store"
)

// maxMCPSessions — сколько сессий MCP держать в памяти; старые вытесняются.
const maxMCPSessions = 64

// mcpSession — подключение внешнего агента по MCP. Каждое попадает в окно
// «Агенты» отдельной записью с журналом вызовов.
type mcpSession struct {
	srv     *mcp.Server
	agentID string
	project string
	used    time.Time
}

// MCP — обработчик /mcp (Streamable HTTP). Проект задаётся параметром
// ?project=<путь> и должен быть добавлен в Edy.
func (h *Handler) MCP() http.Handler {
	return http.HandlerFunc(h.serveMCP)
}

func (h *Handler) serveMCP(w http.ResponseWriter, r *http.Request) {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(h.Token)) != 1 {
		fail(w, http.StatusUnauthorized, "нужен токен OmniRoute")
		return
	}
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		h.mcpM.Lock()
		if s := h.mcpS[r.Header.Get("Mcp-Session-Id")]; s != nil {
			delete(h.mcpS, r.Header.Get("Mcp-Session-Id"))
			h.Store.FinishAgent(s.agentID, store.AgentDone, "Клиент отключился", "")
		}
		h.mcpM.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		// Поток уведомлений от сервера (GET) не нужен: Edy их не шлёт.
		w.Header().Set("Allow", "POST, DELETE")
		fail(w, http.StatusMethodNotAllowed, "поддерживается только POST")
		return
	}
	project := r.URL.Query().Get("project")
	if project == "" {
		fail(w, http.StatusBadRequest, "укажите проект: /mcp?project=<путь>")
		return
	}
	if !h.knownProject(w, project) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		fail(w, http.StatusBadRequest, "не удалось прочитать запрос")
		return
	}
	body = bytes.TrimSpace(body)

	var head struct {
		Method string `json:"method"`
		Params struct {
			ClientInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"clientInfo"`
		} `json:"params"`
	}
	json.Unmarshal(body, &head)

	var sess *mcpSession
	if head.Method == "initialize" {
		sess = h.newMCPSession(project, strings.TrimSpace(head.Params.ClientInfo.Name+" "+head.Params.ClientInfo.Version))
		w.Header().Set("Mcp-Session-Id", sess.agentID)
	} else if id := r.Header.Get("Mcp-Session-Id"); id != "" {
		h.mcpM.Lock()
		sess = h.mcpS[id]
		if sess != nil {
			sess.used = time.Now()
		}
		h.mcpM.Unlock()
		if sess == nil || sess.project != project {
			// Сессия забыта (например, Edy перезапускали) — клиент должен
			// заново выполнить initialize.
			fail(w, http.StatusNotFound, "сессия MCP не найдена — переподключитесь")
			return
		}
	} else {
		// Клиент без сессий: обслуживаем без журнала.
		sess = &mcpSession{srv: h.mcpServer(project, ""), project: project}
	}

	resp := sess.srv.Handle(r.Context(), body)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(resp)
}

func (h *Handler) mcpServer(project, agentID string) *mcp.Server {
	s := &mcp.Server{Root: project, Home: h.Home}
	if h.Tasks != nil {
		s.Busy = func() bool { return h.Tasks.Running(project) }
	}
	if agentID != "" {
		s.OnCall = func(tool, note string, isErr bool) {
			kind := "tool"
			if isErr {
				kind = "error"
			}
			h.Store.AddAgentEvent(agentID, kind, tool+" "+note, time.Now())
		}
		s.OnChange = func(step string) {
			h.Store.AddAgentEvent(agentID, "checkpoint", "Снимок — "+step, time.Now())
		}
	}
	return s
}

func (h *Handler) newMCPSession(project, client string) *mcpSession {
	if client == "" {
		client = "MCP-клиент"
	}
	id := "mcp-" + randomHex(8)
	sess := &mcpSession{srv: h.mcpServer(project, id), agentID: id, project: project, used: time.Now()}
	h.Store.CreateAgent(store.Agent{ID: id, Role: store.RoleMCP, Model: client, Project: project,
		Goal: "Внешний агент работает с проектом через MCP"})

	h.mcpM.Lock()
	defer h.mcpM.Unlock()
	if h.mcpS == nil {
		h.mcpS = map[string]*mcpSession{}
	}
	if len(h.mcpS) >= maxMCPSessions {
		old := make([]*mcpSession, 0, len(h.mcpS))
		for _, s := range h.mcpS {
			old = append(old, s)
		}
		sort.Slice(old, func(i, j int) bool { return old[i].used.Before(old[j].used) })
		for _, s := range old[:len(old)-maxMCPSessions+1] {
			delete(h.mcpS, s.agentID)
			h.Store.FinishAgent(s.agentID, store.AgentDone, "Сессия закрыта", "")
		}
	}
	h.mcpS[id] = sess
	return sess
}

// CloseMCP отмечает подключённых MCP-клиентов завершёнными (при выходе из Edy).
func (h *Handler) CloseMCP() {
	h.mcpM.Lock()
	defer h.mcpM.Unlock()
	for id := range h.mcpS {
		h.Store.FinishAgent(id, store.AgentDone, "Edy закрыт", "")
		delete(h.mcpS, id)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- Скиллы ----

type skillsJSON struct {
	Skills     []skills.Skill `json:"skills"`
	GlobalDir  string         `json:"global_dir"`
	ProjectDir string         `json:"project_dir,omitempty"`
}

func (h *Handler) registerSkillRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/skills", h.listSkills)
	m.HandleFunc("POST /api/skills", h.saveSkill)
	m.HandleFunc("DELETE /api/skills", h.deleteSkill)
}

// skillDirs возвращает папки скиллов; project пустой — только общие.
func (h *Handler) skillDirs(w http.ResponseWriter, project string) ([]skills.Dir, bool) {
	if project != "" && !h.knownProject(w, project) {
		return nil, false
	}
	return skills.Dirs(h.Home, project), true
}

func (h *Handler) listSkills(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	dirs, ok := h.skillDirs(w, project)
	if !ok {
		return
	}
	list, err := skills.Load(dirs)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	res := skillsJSON{Skills: list, GlobalDir: skills.Dirs(h.Home, "")[0].Path}
	if project != "" {
		res.ProjectDir = dirs[len(dirs)-1].Path
	}
	reply(w, res)
}

func (h *Handler) saveSkill(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Project     string `json:"project"`
		Scope       string `json:"scope"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	dir, ok := h.skillScopeDir(w, req.Scope, req.Project)
	if !ok {
		return
	}
	path, err := skills.Save(dir, skills.Skill{Name: strings.TrimSpace(req.Name), Description: req.Description, Body: req.Body})
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	replyStatus(w, http.StatusCreated, map[string]string{"path": path})
}

func (h *Handler) deleteSkill(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dir, ok := h.skillScopeDir(w, q.Get("scope"), q.Get("project"))
	if !ok {
		return
	}
	list, err := skills.Load([]skills.Dir{{Path: dir}})
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	sk, found := skills.Find(list, q.Get("name"))
	if !found {
		fail(w, http.StatusNotFound, fmt.Sprintf("скилл %q не найден", q.Get("name")))
		return
	}
	if err := skills.Remove(sk); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	reply(w, map[string]bool{"ok": true})
}

func (h *Handler) skillScopeDir(w http.ResponseWriter, scope, project string) (string, bool) {
	switch scope {
	case skills.Global:
		return skills.Dirs(h.Home, "")[0].Path, true
	case skills.Project:
		if project == "" {
			fail(w, http.StatusBadRequest, "не выбран проект")
			return "", false
		}
		dirs, ok := h.skillDirs(w, project)
		if !ok {
			return "", false
		}
		return dirs[len(dirs)-1].Path, true
	}
	fail(w, http.StatusBadRequest, "scope: global или project")
	return "", false
}
