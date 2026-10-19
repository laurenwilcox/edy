// Package api — JSON API для интерфейса Edy (браузер или окно Wails).
// Секреты ключей наружу не отдаются никогда: только метки.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/laurenwilcox/name/internal/agent"
	"github.com/laurenwilcox/name/internal/logbuf"
	"github.com/laurenwilcox/name/internal/notify"
	"github.com/laurenwilcox/name/internal/omniroute"
	"github.com/laurenwilcox/name/internal/semantic"
	"github.com/laurenwilcox/name/internal/store"
	"github.com/laurenwilcox/name/internal/tasks"
	"github.com/laurenwilcox/name/internal/webchat"
)

// WebManager — то, что API нужно от менеджера браузеров.
type WebManager interface {
	Sites() (map[string]webchat.Site, error)
	Login(ctx context.Context, c webchat.Conn) error
	Check(ctx context.Context, c webchat.Conn) webchat.Status
	Status(profile string) webchat.Status
	RemoveProfile(name string) error
	BrowserInstalled(kind string) bool
	Features(ctx context.Context, c webchat.Conn) (webchat.Features, error)
	CachedFeatures(profile string) (webchat.Features, bool)
	Options(profile string) webchat.Options
	SaveOptions(profile, site string, o webchat.Options) error
	Usage(profile, site string) webchat.Usage
}

const installTimeout = 10 * time.Minute

// Handler обслуживает /api/*. Каждый запрос должен нести Bearer-токен
// OmniRoute: так сторонний сайт в браузере не сможет управлять Edy через localhost.
type Handler struct {
	Store *store.Store
	Token string
	Addr  string // адрес OmniRoute для экрана «Подключение»

	// OpenURL открывает ссылку в браузере; подменяется в тестах.
	OpenURL func(link string) error

	// FindExecutable, Install и ListModels подменяются в тестах.
	FindExecutable func(name string) (string, error)
	Install        func(ctx context.Context, command string) (output string, err error)
	ListModels     func(ctx context.Context, p store.Provider, key string) ([]string, error)

	// Web — менеджер браузеров для веб-чатов; nil, если веб-чаты выключены.
	Web WebManager
	// LLM — доступ к моделям через OmniRoute для Оркестратора; Tasks — фоновые задачи агента.
	LLM   agent.LLM
	Tasks *tasks.Manager
	// asst — разговоры ИИ-помощника интерфейса.
	asst asstSessions
	// ReposDir — куда клонировать репозитории с GitHub, GitLab и Gitea.
	ReposDir string
	// Home — папка данных Edy (общие скиллы в Home/skills).
	Home string
	// Logs — журнал работы для экрана «Логи»; nil — журнала нет.
	Logs *logbuf.Buffer
	// Notify показывает системное уведомление; подменяется в тестах.
	Notify notify.Notifier
	// Semantic — поиск по смыслу (эмбеддинги); nil — только поиск по словам.
	Semantic *semantic.Engine
	// Limits — лимиты провайдеров, замеченные OmniRoute; nil — только заданные вручную.
	Limits LimitSource

	mux  *routeMux
	mcpM sync.Mutex
	mcpS map[string]*mcpSession
	mcpB map[string]*mcpBinding
}

func New(st *store.Store, token, addr string) *Handler {
	h := &Handler{Store: st, Token: token, Addr: addr, FindExecutable: omniroute.FindExecutable, Install: runInstall, ListModels: omniroute.ListModels, OpenURL: openInBrowser, Notify: notify.System}
	m := &routeMux{ServeMux: http.NewServeMux()}
	m.HandleFunc("GET /api/info", h.info)
	h.semanticRoutes(m)
	m.HandleFunc("GET /api/presets", h.presets)
	m.HandleFunc("GET /api/providers", h.listProviders)
	m.HandleFunc("GET /api/providers/limits", h.limits)
	m.HandleFunc("PUT /api/providers/{name}/limits", h.setLimits)
	m.HandleFunc("POST /api/providers/{name}/check", h.checkLimits)
	m.HandleFunc("POST /api/providers", h.addProvider)
	m.HandleFunc("DELETE /api/providers/{name}", h.deleteProvider)
	m.HandleFunc("POST /api/providers/{name}/keys", h.addKey)
	m.HandleFunc("GET /api/providers/{name}/remote-models", h.remoteModels)
	m.HandleFunc("PUT /api/providers/{name}/models", h.setModels)
	m.HandleFunc("PUT /api/providers/{name}/caps", h.setCaps)
	m.HandleFunc("PUT /api/providers/{name}/window", h.setShowWindow)
	m.HandleFunc("GET /api/caps", h.capsCatalog)
	m.HandleFunc("GET /api/prefs", h.prefs)
	m.HandleFunc("PUT /api/prefs", h.setPrefs)
	m.HandleFunc("POST /api/notify/test", h.testNotify)
	m.HandleFunc("DELETE /api/keys/{id}", h.deleteKey)
	m.HandleFunc("POST /api/install", h.install)
	m.HandleFunc("POST /api/web/login", h.webLogin)
	m.HandleFunc("POST /api/web/check", h.webCheck)
	m.HandleFunc("GET /api/web/status", h.webStatus)
	m.HandleFunc("GET /api/web/browsers", h.webBrowsers)
	m.HandleFunc("POST /api/open", h.openURL)
	m.HandleFunc("GET /api/usage", h.usage)
	m.HandleFunc("PUT /api/budget", h.setBudget)
	m.HandleFunc("POST /api/micro", h.microRun)
	m.HandleFunc("GET /api/micro/settings", h.microSettings)
	m.HandleFunc("PUT /api/micro/settings", h.microSettings)
	m.HandleFunc("GET /api/ui/prefs", h.uiPrefsHandler)
	m.HandleFunc("PUT /api/ui/prefs", h.uiPrefsHandler)
	m.HandleFunc("POST /api/ui/bot", h.uiBot)
	m.HandleFunc("GET /api/ui/css", h.uiCSS)
	m.HandleFunc("PUT /api/ui/css", h.uiCSS)
	m.HandleFunc("DELETE /api/ui/css", h.uiCSS)
	m.HandleFunc("POST /api/assistant", h.assistant)
	m.HandleFunc("POST /api/assistant/confirm", h.assistantConfirm)
	m.HandleFunc("GET /api/assistant/settings", h.assistantSettings)
	m.HandleFunc("PUT /api/assistant/settings", h.assistantSettings)
	m.HandleFunc("DELETE /api/assistant/{session}", h.assistantReset)
	m.HandleFunc("GET /api/logs", h.logs)
	h.registerWebFeatureRoutes(m)
	h.registerProjectRoutes(m)
	h.registerGitRoutes(m)
	h.registerSkillRoutes(m)
	h.registerMCPCatalogRoutes(m)
	h.registerMemoryRoutes(m)
	h.registerTeamRoutes(m)
	h.mux = m
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(h.Token)) != 1 {
		fail(w, http.StatusUnauthorized, "нужен токен OmniRoute")
		return
	}
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) info(w http.ResponseWriter, r *http.Request) {
	reply(w, map[string]string{"base_url": "http://" + h.Addr + "/v1", "token": h.Token,
		"mcp_url": "http://" + h.Addr + "/mcp", "edy_path": edyPath()})
}

// edyPath — путь к консольной команде edy для настройки MCP по stdio. В
// Edy.app окно — Contents/MacOS/Edy, а команда лежит в Contents/Resources/bin.
func edyPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "edy"
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	if dir := filepath.Dir(exe); filepath.Base(dir) == "MacOS" {
		cli := filepath.Join(dir, "..", "Resources", "bin", "edy")
		if _, err := os.Stat(cli); err == nil {
			return filepath.Clean(cli)
		}
	}
	return exe
}

type cliPresetJSON struct {
	Name    string   `json:"name"`
	Title   string   `json:"title"`
	Command []string `json:"command"`
	Install string   `json:"install"`
	Note    string   `json:"note"`
}

type webPresetJSON struct {
	Name           string `json:"name"`
	Title          string `json:"title"`
	URL            string `json:"url"`
	MinIntervalSec int    `json:"min_interval_sec"`
	DailyLimit     int    `json:"daily_limit"`
}

type apiPresetJSON struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Kind    string `json:"kind"`
	BaseURL string `json:"base_url"`
	KeyURL  string `json:"key_url"`
	Note    string `json:"note"`
}

func (h *Handler) presets(w http.ResponseWriter, r *http.Request) {
	out := struct {
		CLI []cliPresetJSON `json:"cli"`
		API []apiPresetJSON `json:"api"`
		Web []webPresetJSON `json:"web"`
	}{CLI: []cliPresetJSON{}, API: []apiPresetJSON{}, Web: []webPresetJSON{}}
	if h.Web != nil {
		sites, err := h.Web.Sites()
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		for name, s := range sites {
			out.Web = append(out.Web, webPresetJSON{Name: name, Title: s.Title, URL: s.URL, MinIntervalSec: s.MinIntervalSec, DailyLimit: s.DailyLimit})
		}
		sort.Slice(out.Web, func(i, j int) bool { return out.Web[i].Title < out.Web[j].Title })
	}
	for name, p := range omniroute.CLIPresets {
		out.CLI = append(out.CLI, cliPresetJSON{Name: name, Title: p.Title, Command: p.Command, Install: p.Install, Note: p.Note})
	}
	for name, p := range omniroute.APIPresets {
		out.API = append(out.API, apiPresetJSON{Name: name, Title: p.Title, Kind: p.Kind, BaseURL: p.BaseURL, KeyURL: p.KeyURL, Note: p.Note})
	}
	sort.Slice(out.CLI, func(i, j int) bool { return out.CLI[i].Title < out.CLI[j].Title })
	sort.Slice(out.API, func(i, j int) bool { return out.API[i].Title < out.API[j].Title })
	reply(w, out)
}

// remoteModels спрашивает у провайдера список моделей первым рабочим ключом.
func (h *Handler) remoteModels(w http.ResponseWriter, r *http.Request) {
	p, err := h.Store.Provider(r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "провайдер не найден")
		return
	} else if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	keys, err := h.Store.Keys(p.ID)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	key := ""
	if len(keys) > 0 {
		key = keys[0].Secret
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	models, err := h.ListModels(ctx, p, key)
	if err != nil {
		fail(w, http.StatusBadGateway, "не удалось получить список моделей: "+err.Error())
		return
	}
	reply(w, map[string][]string{"models": models})
}

func (h *Handler) setModels(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Models []string `json:"models"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	var models []string
	for _, m := range req.Models {
		if m = strings.TrimSpace(m); m != "" && !strings.Contains(m, ",") {
			models = append(models, m)
		}
	}
	if err := h.Store.SetModels(r.PathValue("name"), models); errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "провайдер не найден")
	} else if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

type providerJSON struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	BaseURL  string   `json:"base_url"`
	Models   []string `json:"models"`
	PriceIn  float64  `json:"price_in"`
	PriceOut float64  `json:"price_out"`
	Command  []string `json:"command"`
	Site     string   `json:"site"`
	Browser  string   `json:"browser"`
	// ShowWindow — веб-провайдер работает в видимом окне браузера.
	ShowWindow bool            `json:"show_window"`
	Keys       []store.KeyInfo `json:"keys"`
	// MCP — исполнитель задач получает MCP-сервер Edy напрямую (Claude Code).
	MCP bool `json:"mcp"`
	// Caps — действующие возможности; CapsAuto — определены автоматически.
	Caps     []string `json:"caps"`
	CapsAuto bool     `json:"caps_auto"`
	// CanMCP — утилита технически умеет подключать MCP (тег можно включить).
	CanMCP bool `json:"can_mcp"`
}

// SettingNotifyOff — «1», если уведомления выключены.
const SettingNotifyOff = "notify_off"

func (h *Handler) prefs(w http.ResponseWriter, r *http.Request) {
	off, _ := h.Store.Setting(SettingNotifyOff)
	reply(w, map[string]bool{"notify": off != "1"})
}

func (h *Handler) setPrefs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Notify *bool `json:"notify"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	if req.Notify != nil {
		v := "0"
		if !*req.Notify {
			v = "1"
		}
		if err := h.Store.SetSetting(SettingNotifyOff, v); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	h.prefs(w, r)
}

func (h *Handler) testNotify(w http.ResponseWriter, r *http.Request) {
	if err := h.Notify("Edy", "Уведомления работают: так Edy сообщит, что задача готова или нужна помощь"); err != nil {
		fail(w, http.StatusBadGateway, "macOS не показал уведомление: "+err.Error())
		return
	}
	reply(w, map[string]bool{"ok": true})
}

func (h *Handler) capsCatalog(w http.ResponseWriter, r *http.Request) {
	reply(w, omniroute.CapCatalog)
}

// setShowWindow переключает видимость окна браузера веб-провайдера.
// Браузер перезапустится в новом режиме при следующем запросе.
func (h *Handler) setShowWindow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Show bool `json:"show"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "неверный JSON")
		return
	}
	if err := h.Store.SetShowWindow(r.PathValue("name"), req.Show); errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "провайдер не найден")
		return
	} else if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	reply(w, map[string]any{"show_window": req.Show})
}

// setCaps задаёт теги возможностей провайдера; {"caps": null} — вернуть автоопределение.
func (h *Handler) setCaps(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Caps []string `json:"caps"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	for _, c := range req.Caps {
		if omniroute.CapTitle(c) == c {
			fail(w, http.StatusBadRequest, "неизвестная возможность "+c)
			return
		}
	}
	if err := h.Store.SetCaps(r.PathValue("name"), req.Caps); errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "провайдер не найден")
		return
	} else if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	p, _ := h.Store.Provider(r.PathValue("name"))
	reply(w, map[string]any{"caps": omniroute.Caps(p), "caps_auto": p.Caps == nil})
}

func (h *Handler) listProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := h.Store.Providers()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []providerJSON{}
	for _, p := range providers {
		keys, err := h.Store.KeyInfos(p.ID)
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		models := p.Models
		if models == nil {
			models = []string{}
		}
		out = append(out, providerJSON{Name: p.Name, Kind: p.Kind, BaseURL: p.BaseURL, Models: models,
			PriceIn: p.PriceIn, PriceOut: p.PriceOut, Command: p.Command, Site: p.Site, Browser: p.Browser, ShowWindow: p.ShowWindow, Keys: keys,
			MCP: omniroute.HasCap(p, omniroute.CapMCP), Caps: omniroute.Caps(p), CapsAuto: p.Caps == nil,
			CanMCP: p.Kind == store.KindCLI && omniroute.SupportsMCP(p.Command)})
	}
	reply(w, out)
}

type addProviderReq struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	BaseURL  string   `json:"base_url"`
	Models   []string `json:"models"`
	PriceIn  float64  `json:"price_in"`
	PriceOut float64  `json:"price_out"`
	Preset   string   `json:"preset"`
	Command  []string `json:"command"`
	Site     string   `json:"site"`
	Browser  string   `json:"browser"`
	// ShowWindow — показывать окно браузера (по умолчанию скрыто).
	ShowWindow bool `json:"show_window"`
}

// needInstall — ответ 409, когда утилиту профиля надо сначала установить.
type needInstall struct {
	Error   string `json:"error"`
	Tool    string `json:"tool"`
	Install string `json:"install"`
}

func (h *Handler) addProvider(w http.ResponseWriter, r *http.Request) {
	var req addProviderReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	p := store.Provider{Name: strings.TrimSpace(req.Name), Kind: req.Kind, BaseURL: strings.TrimSpace(req.BaseURL),
		PriceIn: req.PriceIn, PriceOut: req.PriceOut}
	for _, m := range req.Models {
		if m = strings.TrimSpace(m); m != "" {
			p.Models = append(p.Models, m)
		}
	}
	if p.Name == "" || strings.Contains(p.Name, "/") {
		fail(w, http.StatusBadRequest, "имя провайдера обязательно и не может содержать «/»")
		return
	}
	if p.PriceIn < 0 || p.PriceOut < 0 {
		fail(w, http.StatusBadRequest, "цена не может быть отрицательной")
		return
	}
	switch p.Kind {
	case store.KindOpenAI, store.KindAnthropic:
	case store.KindCLI:
		switch {
		case req.Preset != "":
			pr, ok := omniroute.CLIPresets[req.Preset]
			if !ok {
				fail(w, http.StatusBadRequest, fmt.Sprintf("неизвестный профиль %q", req.Preset))
				return
			}
			bin, err := h.FindExecutable(pr.Command[0])
			if err != nil {
				replyStatus(w, http.StatusConflict, needInstall{
					Error: fmt.Sprintf("утилита %q не установлена", pr.Command[0]), Tool: req.Preset, Install: pr.Install})
				return
			}
			p.Command = append([]string{bin}, pr.Command[1:]...)
		case len(req.Command) > 0:
			if !strings.Contains(strings.Join(req.Command, " "), "{prompt}") {
				fail(w, http.StatusBadRequest, "в команде нужна подстановка {prompt}")
				return
			}
			p.Command = req.Command
		default:
			fail(w, http.StatusBadRequest, "для CLI-провайдера нужен профиль или команда")
			return
		}
	case store.KindWeb:
		if h.Web == nil {
			fail(w, http.StatusBadRequest, "веб-чаты выключены")
			return
		}
		sites, err := h.Web.Sites()
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		if _, ok := sites[req.Site]; !ok {
			fail(w, http.StatusBadRequest, fmt.Sprintf("неизвестный веб-чат %q", req.Site))
			return
		}
		p.Site = req.Site
		switch req.Browser {
		case webchat.BrowserChromium, webchat.BrowserChrome:
			p.Browser = req.Browser
		case "":
			p.Browser = webchat.BrowserChromium
		default:
			fail(w, http.StatusBadRequest, "браузер должен быть chromium или chrome")
			return
		}
		p.ShowWindow = req.ShowWindow
		if len(p.Models) == 0 {
			p.Models = []string{"chat"} // модель выбирается на самом сайте
		}
	default:
		fail(w, http.StatusBadRequest, "тип должен быть openai, anthropic, cli или web")
		return
	}
	if _, err := h.Store.AddProvider(p); errors.Is(err, store.ErrExists) {
		fail(w, http.StatusConflict, fmt.Sprintf("провайдер %q уже есть", p.Name))
		return
	} else if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	replyStatus(w, http.StatusCreated, map[string]string{"name": p.Name})
}

func (h *Handler) deleteProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p, _ := h.Store.Provider(name)
	if p.Kind == store.KindWeb && h.Web != nil {
		// Вместе с провайдером удаляется его окружение браузера: cookies и вход.
		if err := h.Web.RemoveProfile(name); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if err := h.Store.DeleteProvider(name); errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "провайдер не найден")
	} else if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *Handler) addKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Label  string `json:"label"`
		Secret string `json:"secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	secret := strings.TrimSpace(req.Secret)
	if secret == "" {
		fail(w, http.StatusBadRequest, "пустой ключ")
		return
	}
	p, err := h.Store.Provider(r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "провайдер не найден")
		return
	} else if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	id, err := h.Store.AddKey(p.ID, strings.TrimSpace(req.Label), secret)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	replyStatus(w, http.StatusCreated, map[string]int64{"id": id})
}

func (h *Handler) deleteKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "некорректный id")
		return
	}
	if err := h.Store.DeleteKey(id); errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "ключ не найден")
	} else if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

// install ставит утилиту профиля. Команда берётся только из встроенных
// профилей — произвольную команду через API запустить нельзя.
func (h *Handler) install(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Preset string `json:"preset"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	pr, ok := omniroute.CLIPresets[req.Preset]
	if !ok {
		fail(w, http.StatusBadRequest, fmt.Sprintf("неизвестный профиль %q", req.Preset))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), installTimeout)
	defer cancel()
	output, err := h.Install(ctx, pr.Install)
	if err != nil {
		replyStatus(w, http.StatusBadGateway, map[string]string{"error": "установка не удалась: " + err.Error(), "output": output})
		return
	}
	bin, err := h.FindExecutable(pr.Command[0])
	if err != nil {
		replyStatus(w, http.StatusBadGateway, map[string]string{"error": "установка завершилась, но утилита не найдена", "output": output})
		return
	}
	reply(w, map[string]string{"path": bin, "output": output})
}

// webConn находит веб-провайдера по имени из запроса.
func (h *Handler) webConn(w http.ResponseWriter, r *http.Request) (webchat.Conn, bool) {
	if h.Web == nil {
		fail(w, http.StatusBadRequest, "веб-чаты выключены")
		return webchat.Conn{}, false
	}
	var req struct {
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return webchat.Conn{}, false
	}
	p, err := h.Store.Provider(req.Provider)
	if err != nil || p.Kind != store.KindWeb {
		fail(w, http.StatusNotFound, "веб-провайдер не найден")
		return webchat.Conn{}, false
	}
	return webchat.Conn{Profile: p.Name, Site: p.Site, Browser: p.Browser, Visible: p.ShowWindow}, true
}

// webLogin открывает сайт в обычном окне браузера подключения (без управления),
// чтобы пользователь вошёл в аккаунт. Изолированный Chromium при первом входе
// скачивается — это несколько минут.
func (h *Handler) webLogin(w http.ResponseWriter, r *http.Request) {
	c, ok := h.webConn(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), installTimeout)
	defer cancel()
	if err := h.Web.Login(ctx, c); err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	reply(w, h.Web.Status(c.Profile))
}

// webCheck проверяет, что подключение работает (вход выполнен).
func (h *Handler) webCheck(w http.ResponseWriter, r *http.Request) {
	c, ok := h.webConn(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), installTimeout)
	defer cancel()
	reply(w, h.Web.Check(ctx, c))
}

// webStatus возвращает состояние всех веб-подключений.
func (h *Handler) webStatus(w http.ResponseWriter, r *http.Request) {
	out := map[string]webchat.Status{}
	if h.Web != nil {
		providers, err := h.Store.Providers()
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, p := range providers {
			if p.Kind == store.KindWeb {
				out[p.Name] = h.Web.Status(p.Name)
			}
		}
	}
	reply(w, out)
}

func (h *Handler) webBrowsers(w http.ResponseWriter, r *http.Request) {
	reply(w, map[string]bool{
		webchat.BrowserChromium: h.Web != nil && h.Web.BrowserInstalled(webchat.BrowserChromium),
		webchat.BrowserChrome:   h.Web != nil && h.Web.BrowserInstalled(webchat.BrowserChrome),
	})
}

// openURL открывает ссылку в браузере по умолчанию. Нужно окну Wails:
// встроенный WebView не открывает ссылки target="_blank" сам.
func (h *Handler) openURL(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		fail(w, http.StatusBadRequest, "можно открывать только ссылки http(s)")
		return
	}
	if err := h.OpenURL(u.String()); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func openInBrowser(link string) error {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	return exec.Command(opener, link).Start()
}

func runInstall(ctx context.Context, command string) (string, error) {
	out, err := exec.CommandContext(ctx, "/bin/bash", "-c", command).CombinedOutput()
	return string(out), err
}

type usageJSON struct {
	TodayUSD  float64              `json:"today_usd"`
	BudgetUSD float64              `json:"budget_usd"`
	Items     []store.UsageSummary `json:"items"`
}

func (h *Handler) usage(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	var out usageJSON
	var err error
	if out.TodayUSD, err = h.Store.SpentSince(midnight); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if out.Items, err = h.Store.UsageSince(midnight); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if raw, err := h.Store.Setting(omniroute.SettingDailyBudget); err == nil {
		out.BudgetUSD, _ = strconv.ParseFloat(raw, 64)
	}
	reply(w, out)
}

func (h *Handler) setBudget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		USD float64 `json:"usd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.USD < 0 {
		fail(w, http.StatusBadRequest, "бюджет должен быть неотрицательным числом")
		return
	}
	if err := h.Store.SetSetting(omniroute.SettingDailyBudget, strconv.FormatFloat(req.USD, 'f', -1, 64)); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func reply(w http.ResponseWriter, v any) { replyStatus(w, http.StatusOK, v) }

func replyStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	replyStatus(w, status, map[string]string{"error": msg})
}
