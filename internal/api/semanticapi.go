package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/laurenwilcox/name/internal/codeindex"
	"github.com/laurenwilcox/name/internal/semantic"
)

func (h *Handler) semanticRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/semantic", h.semanticStatus)
	m.HandleFunc("PUT /api/semantic", h.setSemantic)
	m.HandleFunc("POST /api/semantic/index", h.semanticIndex)
	m.HandleFunc("GET /api/semantic/search", h.semanticSearch)
}

func (h *Handler) semanticReady(w http.ResponseWriter) bool {
	if h.Semantic == nil {
		fail(w, http.StatusServiceUnavailable, "поиск по смыслу недоступен")
		return false
	}
	return true
}

// semanticStatus — модель эмбеддингов и состояние индекса проекта.
func (h *Handler) semanticStatus(w http.ResponseWriter, r *http.Request) {
	if !h.semanticReady(w) {
		return
	}
	project := r.URL.Query().Get("project")
	if project == "" {
		reply(w, semantic.Status{Model: h.Semantic.Model()})
		return
	}
	if !h.knownProject(w, project) {
		return
	}
	reply(w, h.Semantic.Status(project))
}

func (h *Handler) setSemantic(w http.ResponseWriter, r *http.Request) {
	if !h.semanticReady(w) {
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	if err := h.Semantic.SetModel(req.Model); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	reply(w, semantic.Status{Model: h.Semantic.Model()})
}

// semanticIndex строит индекс в фоне; прогресс — в GET /api/semantic.
func (h *Handler) semanticIndex(w http.ResponseWriter, r *http.Request) {
	if !h.semanticReady(w) {
		return
	}
	var req struct {
		Project string `json:"project"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	if !h.knownProject(w, req.Project) {
		return
	}
	if h.Semantic.Model() == "" {
		fail(w, http.StatusBadRequest, "сначала выберите модель эмбеддингов")
		return
	}
	h.Semantic.Start(req.Project, req.Project, 30*time.Minute)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(h.Semantic.Status(req.Project))
}

// semanticSearch — гибридный поиск кода (для проверки индекса в интерфейсе).
func (h *Handler) semanticSearch(w http.ResponseWriter, r *http.Request) {
	if !h.semanticReady(w) {
		return
	}
	project := r.URL.Query().Get("project")
	if !h.knownProject(w, project) {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		fail(w, http.StatusBadRequest, "пустой запрос")
		return
	}
	idx, err := codeindex.Build(r.Context(), project)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	hits, note := semantic.Code{E: h.Semantic, Project: project}.Search(idx, q, 10)
	if hits == nil {
		hits = []codeindex.Hit{}
	}
	reply(w, map[string]any{"hits": hits, "note": note})
}
