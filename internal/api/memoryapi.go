package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/laurenwilcox/name/internal/memory"
	"github.com/laurenwilcox/name/internal/store"
)

// Память проекта в интерфейсе: посмотреть, найти, дописать, закрепить, удалить.

func (h *Handler) registerMemoryRoutes(m *routeMux) {
	m.HandleFunc("GET /api/memory", h.listMemory)
	m.HandleFunc("POST /api/memory", h.addMemory)
	m.HandleFunc("PUT /api/memory/{id}", h.pinMemory)
	m.HandleFunc("DELETE /api/memory/{id}", h.deleteMemory)
}

func (h *Handler) listMemory(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	if !h.knownProject(w, project) {
		return
	}
	notes, err := h.Store.Notes(project)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if agent := r.URL.Query().Get("agent"); agent != "" {
		// Личная память агента команды.
		var own []store.Note
		for _, n := range notes {
			if n.Agent == agent {
				own = append(own, n)
			}
		}
		notes = own
		if notes == nil {
			notes = []store.Note{}
		}
	}
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		notes = memory.Search(notes, q, 30)
		if notes == nil {
			notes = []store.Note{}
		}
	}
	// Сколько из памяти увидит Оркестратор: закреплённое всегда.
	reply(w, map[string]any{"notes": notes, "pinned_brief": memory.Pinned(notes, 1500)})
}

func (h *Handler) addMemory(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Project string `json:"project"`
		Text    string `json:"text"`
		Kind    string `json:"kind"`
		Pinned  bool   `json:"pinned"`
		Agent   string `json:"agent"` // заметка в личную память агента
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	if !h.knownProject(w, req.Project) {
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	switch {
	case req.Text == "":
		fail(w, http.StatusBadRequest, "пустая заметка")
		return
	case len([]rune(req.Text)) > memory.MaxNote:
		fail(w, http.StatusBadRequest, "заметка длиннее 400 символов — память для коротких фактов")
		return
	}
	switch req.Kind {
	case store.NoteFact, store.NoteLesson, store.NoteNote:
	default:
		req.Kind = store.NoteNote
	}
	if req.Agent != "" {
		if a, err := h.Store.TeamAgent(req.Agent); err != nil || a.Project != req.Project {
			fail(w, http.StatusBadRequest, "агент команды не найден")
			return
		}
	}
	id, err := h.Store.AddNote(store.Note{Project: req.Project, Kind: req.Kind, Text: req.Text, Source: "user", Pinned: req.Pinned, Agent: req.Agent})
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	replyStatus(w, http.StatusCreated, map[string]int64{"id": id})
}

func (h *Handler) pinMemory(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Project string `json:"project"`
		Pinned  bool   `json:"pinned"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := h.Store.SetNotePinned(req.Project, id, req.Pinned); errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "заметка не найдена")
		return
	} else if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	reply(w, map[string]bool{"ok": true})
}

func (h *Handler) deleteMemory(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := h.Store.DeleteNote(r.URL.Query().Get("project"), id); errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "заметка не найдена")
		return
	} else if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	reply(w, map[string]bool{"ok": true})
}
