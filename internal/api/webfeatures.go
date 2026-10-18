package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/laurenwilcox/name/internal/store"
	"github.com/laurenwilcox/name/internal/webchat"
)

// featuresTimeout — сколько ждать проверки возможностей веб-чата.
const featuresTimeout = 2 * time.Minute

func (h *Handler) registerWebFeatureRoutes(m *routeMux) {
	m.HandleFunc("POST /api/web/features", h.webFeatures)
	m.HandleFunc("GET /api/web/features", h.webCachedFeatures)
	m.HandleFunc("GET /api/web/settings", h.webSettings)
	m.HandleFunc("PUT /api/web/settings", h.webSettings)
	m.HandleFunc("GET /api/web/limits", h.webLimits)
	m.HandleFunc("DELETE /api/logs", h.clearLogs)
}

// webProvider находит веб-провайдера по имени из ?provider=.
func (h *Handler) webProvider(w http.ResponseWriter, name string) (store.Provider, bool) {
	if h.Web == nil {
		fail(w, http.StatusBadRequest, "веб-чаты выключены")
		return store.Provider{}, false
	}
	p, err := h.Store.Provider(name)
	if err != nil || p.Kind != store.KindWeb {
		fail(w, http.StatusNotFound, "веб-провайдер не найден")
		return store.Provider{}, false
	}
	return p, true
}

// webFeatures открывает чат и читает со страницы подписку, модели,
// инструменты и список чатов. Найденные модели становятся моделями провайдера.
func (h *Handler) webFeatures(w http.ResponseWriter, r *http.Request) {
	c, ok := h.webConn(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), featuresTimeout)
	defer cancel()
	f, err := h.Web.Features(ctx, c)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	if len(f.Models) > 0 {
		models := append([]string{"chat"}, f.Models...)
		if err := h.Store.SetModels(c.Profile, models); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	reply(w, f)
}

// webCachedFeatures отдаёт результат последней проверки без открытия браузера.
func (h *Handler) webCachedFeatures(w http.ResponseWriter, r *http.Request) {
	p, ok := h.webProvider(w, r.URL.Query().Get("provider"))
	if !ok {
		return
	}
	f, found := h.Web.CachedFeatures(p.Name)
	reply(w, map[string]any{"found": found, "features": f})
}

// webSettings читает и сохраняет выбранные модель, инструмент и чат.
func (h *Handler) webSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		p, ok := h.webProvider(w, r.URL.Query().Get("provider"))
		if ok {
			reply(w, h.Web.Options(p.Name))
		}
		return
	}
	var req struct {
		Provider string `json:"provider"`
		webchat.Options
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	p, ok := h.webProvider(w, req.Provider)
	if !ok {
		return
	}
	if err := h.Web.SaveOptions(p.Name, p.Site, req.Options); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	reply(w, h.Web.Options(p.Name))
}

// webLimits — сколько запросов к чату ушло сегодня и что сайт пишет о лимите.
func (h *Handler) webLimits(w http.ResponseWriter, r *http.Request) {
	p, ok := h.webProvider(w, r.URL.Query().Get("provider"))
	if !ok {
		return
	}
	u := h.Web.Usage(p.Name, p.Site)
	f, _ := h.Web.CachedFeatures(p.Name)
	reply(w, map[string]any{"day": u.Day, "used": u.Used, "limit": u.Limit, "notice": f.Limit, "plan": f.Plan})
}

// logs отдаёт журнал работы: ?since=N — только строки после N-й.
// DELETE очищает журнал.
func (h *Handler) logs(w http.ResponseWriter, r *http.Request) {
	if h.Logs == nil {
		reply(w, map[string]any{"lines": []string{}, "seq": 0})
		return
	}
	since, _ := strconv.Atoi(r.URL.Query().Get("since"))
	lines, seq := h.Logs.Lines(since)
	reply(w, map[string]any{"lines": lines, "seq": seq})
}

func (h *Handler) clearLogs(w http.ResponseWriter, r *http.Request) {
	if h.Logs != nil {
		h.Logs.Clear()
	}
	w.WriteHeader(http.StatusNoContent)
}
