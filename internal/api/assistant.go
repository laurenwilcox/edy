package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/laurenwilcox/name/internal/agent"
	"github.com/laurenwilcox/name/internal/omniroute"
	"github.com/laurenwilcox/name/internal/store"
)

// ИИ-помощник: плавающий чат интерфейса. Знает все маршруты API (каталог
// собирается при регистрации) и меняет приложение только через них и через
// свой CSS. Опасные вызовы (удаление, ключи, установка, push…) ждут
// подтверждения пользователя в окне.

// SettingAssistantModel — модель помощника; пусто — выбор Edy (см. assistantModels).
const SettingAssistantModel = "assistant.model"

// SettingUICSS — пользовательский CSS поверх стилей интерфейса.
const SettingUICSS = "ui.css"

const (
	maxUICSS          = 64 << 10
	assistantSteps    = 12
	assistantTimeout  = 5 * time.Minute
	assistantMaxReply = 8000
	assistantIdle     = 2 * time.Hour
)

// routeInfo — маршрут API и имя его обработчика.
type routeInfo struct{ Pattern, Handler string }

// routeMux — ServeMux, который запоминает зарегистрированные маршруты:
// из них строится каталог для помощника.
type routeMux struct {
	*http.ServeMux
	routes []routeInfo
}

func (m *routeMux) HandleFunc(pattern string, f func(http.ResponseWriter, *http.Request)) {
	m.routes = append(m.routes, routeInfo{pattern, funcName(f)})
	m.ServeMux.HandleFunc(pattern, f)
}

// funcName — имя метода обработчика без пакета и суффикса "-fm".
func funcName(f any) string {
	fn := runtime.FuncForPC(reflect.ValueOf(f).Pointer())
	if fn == nil {
		return ""
	}
	name := strings.TrimSuffix(fn.Name(), "-fm")
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// ---- свой CSS ----

func (h *Handler) uiCSS(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		css, _ := h.Store.Setting(SettingUICSS)
		reply(w, map[string]string{"css": css})
	case http.MethodPut:
		var req struct {
			CSS string `json:"css"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, "ожидается {css}")
			return
		}
		if len(req.CSS) > maxUICSS {
			fail(w, http.StatusBadRequest, "CSS больше 64 КБ")
			return
		}
		// CSS вставляется через textContent, но закрывающий тег всё равно не пускаем.
		if strings.Contains(strings.ToLower(req.CSS), "</style") {
			fail(w, http.StatusBadRequest, "в CSS не должно быть </style>")
			return
		}
		if err := h.Store.SetSetting(SettingUICSS, req.CSS); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		reply(w, map[string]string{"css": req.CSS})
	case http.MethodDelete:
		if err := h.Store.SetSetting(SettingUICSS, ""); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		reply(w, map[string]string{"css": ""})
	}
}

// ---- сессии ----

type asstSession struct {
	mu    sync.Mutex
	msgs  []agent.Msg      // переписка без системного промпта
	queue []agent.ToolCall // вызовы, которые ещё не выполнены
	seen  time.Time
	// bad — модели, которые отказались работать помощником (например, CLI
	// без прав на свои инструменты): в этой сессии к ним не возвращаемся.
	bad map[string]bool
}

type asstSessions struct {
	mu sync.Mutex
	m  map[string]*asstSession
}

func (s *asstSessions) get(id string, create bool) (string, *asstSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]*asstSession{}
	}
	now := time.Now()
	for k, v := range s.m {
		if now.Sub(v.seen) > assistantIdle {
			delete(s.m, k)
		}
	}
	if ss, ok := s.m[id]; ok {
		ss.seen = now
		return id, ss
	}
	if !create {
		return id, nil
	}
	if id == "" {
		b := make([]byte, 8)
		rand.Read(b)
		id = hex.EncodeToString(b)
	}
	ss := &asstSession{seen: now}
	s.m[id] = ss
	return id, ss
}

func (s *asstSessions) drop(id string) {
	s.mu.Lock()
	delete(s.m, id)
	s.mu.Unlock()
}

// ---- ответы ----

type asstAction struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
}

type asstPending struct {
	Method  string          `json:"method"`
	Path    string          `json:"path"`
	Body    json.RawMessage `json:"body,omitempty"`
	Summary string          `json:"summary"`
}

type asstReply struct {
	Session    string       `json:"session"`
	Reply      string       `json:"reply"`
	Actions    []asstAction `json:"actions"`
	Pending    *asstPending `json:"pending,omitempty"`
	CSSChanged bool         `json:"css_changed"`
}

type apiCallArgs struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body"`
	Why    string          `json:"why"`
}

var apiCallTool = func() agent.ToolDef {
	var t agent.ToolDef
	t.Type = "function"
	t.Function.Name = "api_call"
	t.Function.Description = "Вызвать HTTP API Edy. Возвращает статус и JSON-ответ. Только пути /api/…"
	t.Function.Parameters = json.RawMessage(`{"type":"object","properties":{` +
		`"method":{"type":"string","enum":["GET","POST","PUT","DELETE"]},` +
		`"path":{"type":"string","description":"путь с параметрами запроса, например /api/tasks?project=/Users/me/app"},` +
		`"body":{"type":"object","description":"JSON-тело для POST/PUT"},` +
		`"why":{"type":"string","description":"кратко по-русски: что и зачем делаешь — покажется пользователю при подтверждении"}},` +
		`"required":["method","path"]}`)
	return t
}()

// ---- обработчики ----

func (h *Handler) assistant(w http.ResponseWriter, r *http.Request) {
	if h.LLM == nil {
		fail(w, http.StatusServiceUnavailable, "модели недоступны")
		return
	}
	var req struct {
		Session string `json:"session"`
		Message string `json:"message"`
		Screen  string `json:"screen"`
		Model   string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Message) == "" {
		fail(w, http.StatusBadRequest, "ожидается {message}")
		return
	}
	id, ss := h.asst.get(req.Session, true)
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if len(ss.queue) > 0 {
		// Новое сообщение вместо ответа на подтверждение — считаем отказом.
		for _, c := range ss.queue {
			ss.msgs = append(ss.msgs, agent.Msg{Role: "tool", ToolCallID: c.ID, Content: "Пользователь не подтвердил этот вызов."})
		}
		ss.queue = nil
	}
	ss.msgs = append(ss.msgs, agent.Msg{Role: "user", Content: req.Message})
	h.assistantRun(w, r, id, ss, req.Screen, req.Model)
}

func (h *Handler) assistantConfirm(w http.ResponseWriter, r *http.Request) {
	if h.LLM == nil {
		fail(w, http.StatusServiceUnavailable, "модели недоступны")
		return
	}
	var req struct {
		Session string `json:"session"`
		Approve bool   `json:"approve"`
		Screen  string `json:"screen"`
		Model   string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "ожидается {session, approve}")
		return
	}
	id, ss := h.asst.get(req.Session, false)
	if ss == nil {
		fail(w, http.StatusNotFound, "разговор не найден — начните заново")
		return
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if len(ss.queue) == 0 {
		fail(w, http.StatusConflict, "нечего подтверждать")
		return
	}
	call := ss.queue[0]
	ss.queue = ss.queue[1:]
	out := asstReply{Session: id, Actions: []asstAction{}}
	if req.Approve {
		h.assistantExec(r.Context(), ss, call, &out)
	} else {
		ss.msgs = append(ss.msgs, agent.Msg{Role: "tool", ToolCallID: call.ID, Content: "Пользователь отказал в этом действии. Не повторяй его без новой просьбы."})
	}
	h.assistantLoop(w, r, ss, req.Screen, req.Model, out)
}

// assistantSettings — модель помощника: GET читает, PUT {model} меняет
// (пусто — модель микро-агентов).
func (h *Handler) assistantSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut {
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, "некорректный JSON")
			return
		}
		if err := h.Store.SetSetting(SettingAssistantModel, strings.TrimSpace(req.Model)); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	chosen, _ := h.Store.Setting(SettingAssistantModel)
	effective := ""
	if list := h.assistantModels("", nil); len(list) > 0 {
		effective = list[0]
	}
	reply(w, map[string]string{"model": chosen, "effective": effective})
}

func (h *Handler) assistantReset(w http.ResponseWriter, r *http.Request) {
	h.asst.drop(r.PathValue("session"))
	reply(w, map[string]bool{"ok": true})
}

func (h *Handler) assistantRun(w http.ResponseWriter, r *http.Request, id string, ss *asstSession, screen, model string) {
	h.assistantLoop(w, r, ss, screen, model, asstReply{Session: id, Actions: []asstAction{}})
}

// assistantLoop — цикл «модель → инструменты»: выполняет очередь вызовов,
// останавливается на опасном вызове (pending) или на текстовом ответе.
func (h *Handler) assistantLoop(w http.ResponseWriter, r *http.Request, ss *asstSession, screen, explicit string, out asstReply) {
	ctx, cancel := context.WithTimeout(r.Context(), assistantTimeout)
	defer cancel()
	models := h.assistantModels(explicit, ss.bad)
	if len(models) == 0 {
		fail(w, http.StatusServiceUnavailable, "нет модели для помощника: подключите провайдера")
		return
	}
	model, notice := models[0], ""
	for step := 0; step < assistantSteps; step++ {
		for len(ss.queue) > 0 {
			call := ss.queue[0]
			if p := h.needsConfirm(call); p != nil {
				out.Pending = p
				reply(w, out)
				return
			}
			ss.queue = ss.queue[1:]
			h.assistantExec(ctx, ss, call, &out)
		}
		msgs := append([]agent.Msg{{Role: "system", Content: h.assistantPrompt(screen)}}, ss.msgs...)
		res, err := h.LLM.Chat(ctx, model, msgs, []agent.ToolDef{apiCallTool})
		if err != nil && agent.Hopeless(err) && len(models) > 1 {
			// Модель отказалась работать — переходим к следующей, шаг не тратим.
			if ss.bad == nil {
				ss.bad = map[string]bool{}
			}
			ss.bad[model] = true
			models = models[1:]
			notice = fmt.Sprintf("ℹ️ %s не подходит помощнику: вместо ответа CLI отказалась работать без подтверждений. Отвечает %s.\n\n", ss.badList(), models[0])
			model = models[0]
			step--
			continue
		}
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		ss.msgs = append(ss.msgs, agent.Msg{Role: "assistant", Content: res.Content, ToolCalls: res.ToolCalls})
		if len(res.ToolCalls) == 0 {
			out.Reply = notice + res.Content
			reply(w, out)
			return
		}
		ss.queue = append(ss.queue, res.ToolCalls...)
	}
	// Шаги кончились: незавершённые вызовы закрываем, чтобы история осталась валидной.
	for _, c := range ss.queue {
		ss.msgs = append(ss.msgs, agent.Msg{Role: "tool", ToolCallID: c.ID, Content: "Не выполнено: закончился лимит шагов."})
	}
	ss.queue = nil
	out.Reply = "Остановился: слишком много шагов подряд. Напишите, продолжать ли."
	reply(w, out)
}

// assistantModels — очередь моделей помощника. Первыми — выбранная явно и
// модель из настроек помощника. Дальше — модели с родным вызовом функций
// (API-провайдеры), от дешёвых к дорогим: помощник только и делает, что
// вызывает api_call, а CLI-агенты норовят запускать свои инструменты.
// В конце — модель микро-агентов и остальные CLI. Веб-чаты не годятся.
func (h *Handler) assistantModels(explicit string, bad map[string]bool) []string {
	var out []string
	add := func(m string) {
		if m = strings.TrimSpace(m); m != "" && !bad[m] && !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	add(explicit)
	chosen, _ := h.Store.Setting(SettingAssistantModel)
	add(chosen)
	providers, _ := h.Store.Providers()
	sort.SliceStable(providers, func(i, j int) bool {
		return providers[i].PriceIn+providers[i].PriceOut < providers[j].PriceIn+providers[j].PriceOut
	})
	first := func(p store.Provider) string {
		for _, m := range p.Models {
			if !strings.Contains(strings.ToLower(m), "embed") {
				return p.Name + "/" + m
			}
		}
		return ""
	}
	for _, p := range providers {
		// У CLI «инструменты» — свои, а не api_call Edy, поэтому только API.
		if (p.Kind == store.KindOpenAI || p.Kind == store.KindAnthropic) && omniroute.HasCap(p, omniroute.CapTools) {
			if m := first(p); m != "" {
				add(m)
			}
		}
	}
	add(h.microModel(""))
	for _, p := range providers {
		if p.Kind == store.KindCLI {
			if m := first(p); m != "" {
				add(m)
			}
		}
	}
	return out
}

func (ss *asstSession) badList() string {
	var list []string
	for m := range ss.bad {
		list = append(list, m)
	}
	sort.Strings(list)
	return strings.Join(list, ", ")
}

// parseCall разбирает аргументы api_call и проверяет путь.
func parseCall(c agent.ToolCall) (apiCallArgs, error) {
	var a apiCallArgs
	if c.Function.Name != "api_call" {
		return a, fmt.Errorf("неизвестный инструмент %q", c.Function.Name)
	}
	if err := json.Unmarshal([]byte(c.Function.Arguments), &a); err != nil {
		return a, fmt.Errorf("аргументы не JSON: %v", err)
	}
	a.Method = strings.ToUpper(strings.TrimSpace(a.Method))
	switch a.Method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete:
	default:
		return a, fmt.Errorf("метод %q не поддерживается", a.Method)
	}
	p := strings.TrimSpace(a.Path)
	if !strings.HasPrefix(p, "/api/") || strings.Contains(p, "..") {
		return a, fmt.Errorf("путь должен начинаться с /api/")
	}
	bare, _, _ := strings.Cut(p, "?")
	if strings.HasPrefix(bare, "/api/assistant") || bare == "/api/info" {
		return a, fmt.Errorf("путь %s помощнику недоступен", bare)
	}
	a.Path = p
	if string(a.Body) == "null" {
		a.Body = nil
	}
	return a, nil
}

// needsConfirm — вызов опасный: вернуть описание для окна подтверждения.
// Битые вызовы подтверждения не требуют — они просто вернут модели ошибку.
func (h *Handler) needsConfirm(c agent.ToolCall) *asstPending {
	a, err := parseCall(c)
	if err != nil || !dangerous(a.Method, a.Path) {
		return nil
	}
	sum := strings.TrimSpace(a.Why)
	if sum == "" {
		sum = a.Method + " " + a.Path
	}
	return &asstPending{Method: a.Method, Path: a.Path, Body: a.Body, Summary: sum}
}

func dangerous(method, path string) bool {
	if method == http.MethodGet {
		return false
	}
	bare, _, _ := strings.Cut(path, "?")
	switch {
	case method == http.MethodDelete,
		bare == "/api/install",
		strings.Contains(bare, "/keys"),
		strings.HasPrefix(bare, "/api/git/accounts"),
		strings.HasSuffix(bare, "/restore"),
		strings.HasSuffix(bare, "/revert-file"),
		strings.HasSuffix(bare, "/push"),
		strings.HasSuffix(bare, "/approve"):
		return true
	}
	return false
}

// assistantExec выполняет вызов через внутренний mux (токен не нужен —
// запрос не выходит из процесса) и кладёт ответ в историю.
func (h *Handler) assistantExec(ctx context.Context, ss *asstSession, c agent.ToolCall, out *asstReply) {
	a, err := parseCall(c)
	if err != nil {
		ss.msgs = append(ss.msgs, agent.Msg{Role: "tool", ToolCallID: c.ID, Content: "Ошибка: " + err.Error()})
		return
	}
	var body *bytes.Reader
	if len(a.Body) > 0 {
		body = bytes.NewReader(a.Body)
	} else {
		body = bytes.NewReader(nil)
	}
	req := httptest.NewRequestWithContext(ctx, a.Method, a.Path, body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	text := strings.TrimSpace(rec.Body.String())
	if len(text) > assistantMaxReply {
		text = text[:assistantMaxReply] + "… (ответ обрезан)"
	}
	ss.msgs = append(ss.msgs, agent.Msg{Role: "tool", ToolCallID: c.ID, Content: fmt.Sprintf("HTTP %d\n%s", rec.Code, text)})
	out.Actions = append(out.Actions, asstAction{Method: a.Method, Path: a.Path, Status: rec.Code})
	bare, _, _ := strings.Cut(a.Path, "?")
	if a.Method != http.MethodGet && rec.Code < 300 && (bare == "/api/ui/css" || bare == "/api/ui/prefs") {
		out.CSSChanged = true
	}
}

// ---- системный промпт ----

// bodyHints — тела запросов, которые не очевидны из имени маршрута.
var bodyHints = map[string]string{
	"addProvider":    `{name, kind, models[], price_in, price_out}`,
	"addKey":         `{label, secret}`,
	"setPrefs":       `{notify}`,
	"setBudget":      `{usd}`,
	"saveMCPServer":  `{name, description, account, type, url, command, args[], headers{}, env{}}`,
	"saveSkill":      `{project, scope, name, description, body}`,
	"deleteSkill":    `параметры ?scope=&project=`,
	"addMemory":      `{project, text, kind, pinned, agent}`,
	"addProject":     `{path}`,
	"removeProject":  `{path, id}`,
	"createTask":     `{project, parent, title, request}`,
	"startTask":      `{id, project, parent, title, request, plan, comment, models[], conversation, subtasks, agent}`,
	"microSettings":  `{model}`,
	"gitSettings":    `{path, settings}`,
	"addGitAccount":  `{kind, host, label, username, token}`,
	"saveTeamAgent":  `store.TeamAgent, обязателен project`,
	"setUIPrefs":     `поля theme, accent, font_size, font, sidebar, animations`,
	"uiPrefsHandler": `PUT: поля theme(system|light|dark), accent(indigo|blue|green|orange|pink|red), font_size(12–18), font(system|serif|mono), sidebar(left|right), animations(on|off)`,
	"uiCSS":          `PUT {css} — ЗАМЕНЯЕТ весь пользовательский CSS (сначала GET и дополни); DELETE — сброс`,
}

const assistantIntro = `Ты — встроенный ИИ-помощник Edy, десктопной IDE для работы с ИИ-агентами (macOS, окно на Wails).
Edy подключает модели (API-провайдеры, CLI-утилиты, веб-чаты через Chromium), ведёт проекты, задачи агентов, git, MCP-серверы, память, навыки и команду агентов.
Ты можешь сделать в приложении всё, о чём просит пользователь, через инструмент api_call и пользовательский CSS.

Разделы интерфейса (id панели → что там → основные эндпоинты):
- Чат (go-chat) — основной чат с моделью, задачи агента: /api/tasks, /api/board, /api/templates.
- p-list «Провайдеры» — подключённые модели: /api/providers, /api/providers/{name}/keys, /api/providers/{name}/models.
- p-add «Добавить» — новые провайдеры, пресеты: /api/presets, /api/providers, /api/web/*.
- p-usage «Расходы» — /api/usage, /api/budget, /api/stats/models.
- p-connect «Подключение» — адрес OmniRoute и MCP (токен помощнику не показывается).
- p-project «Проект» — /api/projects, чекпоинты, контекст, файлы, оркестратор, /api/semantic.
- p-git «Git» — /api/git/accounts, /api/git/info, /api/git/settings, /api/git/{action}.
- p-team «Команда» — /api/team.
- p-mcp «MCP» — /api/mcp-servers.
- p-agents «Агенты» — журнал агентов /api/agents, память /api/memory, навыки /api/skills, микро-агенты /api/micro.
- p-look «Оформление» — /api/ui/prefs (тема, акцент, шрифт, меню, анимации) и /api/ui/css (свой CSS).

Оформление: сначала пробуй /api/ui/prefs; то, чего там нет, делай своим CSS через /api/ui/css.
CSS-переменные :root: --bg --bg-2 --glass --glass-strong --glass-border --text --muted --line --a1 --a2 --grad --on-grad --field --code --danger --ok --warn --shadow --blob-opacity.
Тёмная тема переопределяет их под [data-theme="dark"]. Селекторы: .side#side (меню), .menu-item, #drawer (выдвижная панель), main.chat (чат), .chat-head, #composer, #input, .glass.
Пользователь откатывает твой CSS одной кнопкой в «Оформлении», поэтому меняй CSS смело, но аккуратно: дописывай к существующему.

Правила:
- Отвечай только по-русски, коротко и по делу.
- Не знаешь состояния — сначала прочитай (GET), потом меняй. Не выдумывай id и имена — бери их из ответов API.
- Опасные действия (удаление, ключи, установка, git push, откат) пользователь подтверждает в окне — просто вызывай их, заполнив why.
- Секреты (ключи, токены) не пересказывай.
- После изменений кратко скажи, что сделал.`

func (h *Handler) assistantPrompt(screen string) string {
	var b strings.Builder
	b.WriteString(assistantIntro)
	if s := strings.TrimSpace(screen); s != "" {
		b.WriteString("\n\nСейчас у пользователя открыт раздел: " + s + ". «Здесь», «эту страницу» — про него.")
	}
	b.WriteString("\n\nВсе маршруты API (метод путь — обработчик [тело]):\n")
	routes := append([]routeInfo(nil), h.mux.routes...)
	sort.SliceStable(routes, func(i, j int) bool { return routePath(routes[i].Pattern) < routePath(routes[j].Pattern) })
	for _, rt := range routes {
		bare := routePath(rt.Pattern)
		if strings.HasPrefix(bare, "/api/assistant") || bare == "/api/info" {
			continue
		}
		b.WriteString("- " + rt.Pattern + " — " + rt.Handler)
		if hint, ok := bodyHints[rt.Handler]; ok {
			b.WriteString(" " + hint)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func routePath(pattern string) string {
	if _, p, ok := strings.Cut(pattern, " "); ok {
		return p
	}
	return pattern
}
