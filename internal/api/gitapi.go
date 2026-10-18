package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/laurenwilcox/name/internal/agent"
	"github.com/laurenwilcox/name/internal/gitops"
	"github.com/laurenwilcox/name/internal/store"
	"github.com/laurenwilcox/name/internal/tasks"
)

func (h *Handler) registerGitRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/git/accounts", h.gitAccounts)
	m.HandleFunc("POST /api/git/accounts", h.addGitAccount)
	m.HandleFunc("DELETE /api/git/accounts/{id}", h.deleteGitAccount)
	m.HandleFunc("POST /api/git/clone", h.gitClone)
	m.HandleFunc("GET /api/git/info", h.gitInfo)
	m.HandleFunc("PUT /api/git/settings", h.gitSettings)
	m.HandleFunc("POST /api/git/{action}", h.gitRemoteAction)
	m.HandleFunc("GET /api/agents", h.agents)
	m.HandleFunc("GET /api/agents/{id}", h.agentDetail)
}

func (h *Handler) gitAccounts(w http.ResponseWriter, r *http.Request) {
	list, err := h.Store.GitAccounts()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	reply(w, list)
}

func (h *Handler) addGitAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind     string `json:"kind"`
		Host     string `json:"host"`
		Label    string `json:"label"`
		Username string `json:"username"`
		Token    string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	a := store.GitAccount{Kind: req.Kind, Host: strings.TrimRight(strings.TrimSpace(req.Host), "/"), Label: strings.TrimSpace(req.Label), Username: strings.TrimSpace(req.Username), Token: strings.TrimSpace(req.Token)}
	switch a.Kind {
	case store.GitHub:
		if a.Host == "" {
			a.Host = "https://github.com"
		}
	case store.GitLab, store.Gitea:
		if a.Host == "" {
			if a.Kind == store.GitLab {
				a.Host = "https://gitlab.com"
			} else {
				fail(w, http.StatusBadRequest, "для Gitea укажите адрес сервера, например https://gitea.example.com")
				return
			}
		}
	default:
		fail(w, http.StatusBadRequest, "хостинг должен быть github, gitlab или gitea")
		return
	}
	if u, err := url.Parse(a.Host); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		fail(w, http.StatusBadRequest, "адрес сервера должен начинаться с https://")
		return
	}
	if a.Token == "" {
		fail(w, http.StatusBadRequest, "нужен токен доступа")
		return
	}
	if a.Label == "" {
		a.Label = a.Kind
	}
	id, err := h.Store.AddGitAccount(a)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	replyStatus(w, http.StatusCreated, map[string]int64{"id": id})
}

func (h *Handler) deleteGitAccount(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := h.Store.DeleteGitAccount(id); errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "аккаунт не найден")
	} else if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

// account возвращает аккаунт с токеном или nil, если id = 0.
func (h *Handler) account(id int64) (*store.GitAccount, error) {
	if id == 0 {
		return nil, nil
	}
	a, err := h.Store.GitAccount(id)
	if err != nil {
		return nil, fmt.Errorf("аккаунт git: %w", err)
	}
	return &a, nil
}

// gitClone клонирует репозиторий в папку Edy и добавляет его в проекты.
func (h *Handler) gitClone(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL       string `json:"url"`
		AccountID int64  `json:"account_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	if h.ReposDir == "" {
		fail(w, http.StatusServiceUnavailable, "клонирование недоступно")
		return
	}
	name, err := gitops.RepoName(strings.TrimSpace(req.URL))
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	acc, err := h.account(req.AccountID)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	dest := filepath.Join(h.ReposDir, name)
	for i := 2; ; i++ {
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			break
		}
		dest = filepath.Join(h.ReposDir, fmt.Sprintf("%s-%d", name, i))
	}
	ctx, cancel := context.WithTimeout(r.Context(), installTimeout)
	defer cancel()
	if err := gitops.Clone(ctx, strings.TrimSpace(req.URL), dest, acc); err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	paths := []string{dest}
	for _, p := range h.projectPaths() {
		if len(paths) < 10 {
			paths = append(paths, p)
		}
	}
	h.saveProjectPaths(paths)
	if req.AccountID != 0 {
		ps := store.DefaultProjectSettings()
		ps.AccountID = req.AccountID
		h.Store.SetProjectSettings(dest, ps)
	}
	reply(w, h.projectInfo(r.Context(), dest))
}

type gitInfoJSON struct {
	gitops.Info
	Settings store.ProjectSettings `json:"settings"`
}

func (h *Handler) gitInfo(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if !h.knownProject(w, path) {
		return
	}
	ps, err := h.Store.ProjectSettings(path)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	reply(w, gitInfoJSON{Info: gitops.GetInfo(r.Context(), path), Settings: ps})
}

func (h *Handler) gitSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path     string                `json:"path"`
		Settings store.ProjectSettings `json:"settings"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	if !h.knownProject(w, req.Path) {
		return
	}
	ps := req.Settings
	switch ps.Commit {
	case store.CommitBranch, store.CommitDirect, store.CommitNone:
	default:
		fail(w, http.StatusBadRequest, "режим коммита: branch, direct или none")
		return
	}
	switch ps.Push {
	case store.PushOff, store.PushOn, store.PushPR:
	default:
		fail(w, http.StatusBadRequest, "после коммита: off, push или pr")
		return
	}
	if ps.Push == store.PushPR && ps.Commit != store.CommitBranch {
		fail(w, http.StatusBadRequest, "Pull Request возможен только при коммите в новую ветку")
		return
	}
	if ps.Push == store.PushPR && ps.AccountID == 0 {
		fail(w, http.StatusBadRequest, "для Pull Request выберите аккаунт")
		return
	}
	if (ps.AuthorName == "") != (ps.AuthorEmail == "") {
		fail(w, http.StatusBadRequest, "для своего автора коммитов укажите и имя, и email")
		return
	}
	if _, err := h.account(ps.AccountID); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if ps.ReviewModel != "" && ps.ReviewModel != "off" {
		if p, _, ok := splitModelName(ps.ReviewModel); !ok {
			fail(w, http.StatusBadRequest, "ревьюер: модель в виде провайдер/модель")
			return
		} else if _, err := h.Store.Provider(p); err != nil {
			fail(w, http.StatusBadRequest, "ревьюер: неизвестный провайдер "+p)
			return
		}
	}
	if err := h.Store.SetProjectSettings(req.Path, ps); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// gitRemoteAction — Pull и Push текущей ветки проекта.
func (h *Handler) gitRemoteAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	if !h.knownProject(w, req.Path) {
		return
	}
	if h.Tasks != nil && h.Tasks.Running(req.Path) {
		fail(w, http.StatusConflict, "в проекте идёт задача — дождитесь её")
		return
	}
	ps, _ := h.Store.ProjectSettings(req.Path)
	acc, err := h.account(ps.AccountID)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	var out string
	switch r.PathValue("action") {
	case "pull":
		out, err = gitops.Pull(ctx, req.Path, acc)
	case "push":
		err = gitops.Push(ctx, req.Path, "", acc)
		out = "Отправлено в origin"
	default:
		fail(w, http.StatusNotFound, "неизвестное действие")
		return
	}
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	reply(w, map[string]string{"output": out})
}

// Finalizer — Approve по настройкам проекта: коммит, push, Pull/Merge Request.
func Finalizer(st *store.Store) tasks.FinalizeFunc {
	return func(ctx context.Context, project, goal, summary, model string) (tasks.Outcome, error) {
		ps, err := st.ProjectSettings(project)
		if err != nil {
			return tasks.Outcome{}, err
		}
		var acc *store.GitAccount
		if ps.AccountID != 0 {
			a, err := st.GitAccount(ps.AccountID)
			if err != nil {
				return tasks.Outcome{}, fmt.Errorf("аккаунт git из настроек проекта: %w", err)
			}
			acc = &a
		}
		title := strings.TrimSpace(strings.SplitN(goal, "\n", 2)[0])
		res, err := gitops.Finalize(ctx, project, ps, acc, agent.FreeBranch(ctx, project, agent.BranchName(goal, time.Now())), title, summary+"\n\nМодель: "+model)
		if err != nil {
			return tasks.Outcome{}, err
		}
		branch := ""
		if res.Committed {
			branch = res.Branch
		}
		return tasks.Outcome{Branch: branch, PRURL: res.PRURL, Note: res.Note}, nil
	}
}

func (h *Handler) agents(w http.ResponseWriter, r *http.Request) {
	list, err := h.Store.Agents(200)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	reply(w, list)
}

func (h *Handler) agentDetail(w http.ResponseWriter, r *http.Request) {
	a, err := h.Store.Agent(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "агент не найден")
		return
	} else if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	events, err := h.Store.AgentEvents(a.ID, after)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	reply(w, map[string]any{"agent": a, "events": events})
}
