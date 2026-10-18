package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/laurenwilcox/name/internal/store"
)

// Каталог внешних MCP-серверов: из него Оркестратор выбирает, что выдать
// исполнителю под задачу.

// mcpPreset — шаблон популярного сервера для формы «Добавить».
type mcpPreset struct {
	Name        string   `json:"name"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Type        string   `json:"type"`
	URL         string   `json:"url,omitempty"`
	Command     string   `json:"command,omitempty"`
	Args        []string `json:"args,omitempty"`
	Secret      string   `json:"secret,omitempty"` // какой заголовок или переменную заполнить
	Note        string   `json:"note,omitempty"`
}

// Команды запуска сверены с документацией серверов; если у пользователя
// другая версия, их можно поправить в форме.
var mcpPresets = []mcpPreset{
	{Name: "playwright", Title: "Playwright — браузер", Type: store.MCPStdio, Command: "npx", Args: []string{"@playwright/mcp@latest"},
		Description: "Браузер: открыть страницу, кликнуть, заполнить форму, сделать скриншот. Нужен, чтобы проверить веб-интерфейс.",
		Note:        "Нужен Node.js."},
	{Name: "context7", Title: "Context7 — документация библиотек", Type: store.MCPStdio, Command: "npx", Args: []string{"-y", "@upstash/context7-mcp"},
		Description: "Свежая документация и примеры для библиотек и фреймворков. Нужен, когда задача использует незнакомую или новую библиотеку.",
		Note:        "Нужен Node.js."},
	{Name: "fetch", Title: "Fetch — загрузка страниц", Type: store.MCPStdio, Command: "uvx", Args: []string{"mcp-server-fetch"},
		Description: "Скачать веб-страницу или документ по ссылке и получить текст. Нужен, когда в задаче есть ссылка на документацию или спецификацию.",
		Note:        "Нужен uv (brew install uv)."},
	{Name: "github", Title: "GitHub", Type: store.MCPHTTP, URL: "https://api.githubcopilot.com/mcp/", Secret: "Authorization: Bearer <токен GitHub>",
		Description: "Issues, pull requests и код на GitHub. Нужен, когда задача ссылается на issue или PR.",
		Note:        "Токен GitHub (fine-grained) с доступом к нужным репозиториям."},
}

// mcpServerJSON — сервер для интерфейса: значения заголовков и переменных
// окружения не отдаются, только их имена.
type mcpServerJSON struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Account     string   `json:"account,omitempty"`
	Enabled     bool     `json:"enabled"`
	Type        string   `json:"type"`
	URL         string   `json:"url,omitempty"`
	Command     string   `json:"command,omitempty"`
	Args        []string `json:"args,omitempty"`
	Headers     []string `json:"headers,omitempty"`
	Env         []string `json:"env,omitempty"`
}

func (h *Handler) registerMCPCatalogRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/mcp-servers", h.listMCPServers)
	m.HandleFunc("POST /api/mcp-servers", h.saveMCPServer)
	m.HandleFunc("PUT /api/mcp-servers/{name}/enabled", h.toggleMCPServer)
	m.HandleFunc("DELETE /api/mcp-servers/{name}", h.deleteMCPServer)
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func (h *Handler) listMCPServers(w http.ResponseWriter, r *http.Request) {
	list, err := h.Store.MCPServers()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []mcpServerJSON{}
	for _, m := range list {
		out = append(out, mcpServerJSON{Name: m.Name, Description: m.Description, Account: m.Account, Enabled: m.Enabled, Type: m.Type,
			URL: m.URL, Command: m.Command, Args: m.Args, Headers: keysOf(m.Headers), Env: keysOf(m.Env)})
	}
	reply(w, map[string]any{"servers": out, "presets": mcpPresets})
}

func (h *Handler) saveMCPServer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Account     string `json:"account"`
		Type        string `json:"type"`
		URL         string `json:"url"`
		Command     string `json:"command"`
		Args        string `json:"args"`    // по одному аргументу в строке
		Headers     string `json:"headers"` // «Имя: значение» в строке
		Env         string `json:"env"`     // «ИМЯ=значение» в строке
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	m := store.MCPServer{Name: strings.TrimSpace(req.Name), Description: req.Description, Account: req.Account, Enabled: true,
		MCPConfig: store.MCPConfig{Type: req.Type, URL: strings.TrimSpace(req.URL), Command: strings.TrimSpace(req.Command)}}
	for _, line := range strings.Split(req.Args, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			m.Args = append(m.Args, line)
		}
	}
	var err error
	if m.Headers, err = pairs(req.Headers, ":"); err != nil {
		fail(w, http.StatusBadRequest, "заголовки: "+err.Error())
		return
	}
	if m.Env, err = pairs(req.Env, "="); err != nil {
		fail(w, http.StatusBadRequest, "переменные окружения: "+err.Error())
		return
	}
	if strings.TrimSpace(m.Description) == "" {
		fail(w, http.StatusBadRequest, "опишите, для чего сервер: по описанию Оркестратор решает, выдавать ли его")
		return
	}
	if err := h.Store.SaveMCPServer(m); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	replyStatus(w, http.StatusCreated, map[string]string{"name": m.Name})
}

// pairs разбирает строки «ключ<sep>значение».
func pairs(text, sep string) (map[string]string, error) {
	var out map[string]string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, sep)
		if !ok || strings.TrimSpace(k) == "" {
			return nil, errors.New("строка «" + line + "»: ожидается ключ" + sep + "значение")
		}
		if out == nil {
			out = map[string]string{}
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

func (h *Handler) toggleMCPServer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "некорректный JSON")
		return
	}
	if err := h.Store.SetMCPServerEnabled(r.PathValue("name"), req.Enabled); errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "сервер не найден")
		return
	} else if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	reply(w, map[string]bool{"ok": true})
}

func (h *Handler) deleteMCPServer(w http.ResponseWriter, r *http.Request) {
	if err := h.Store.DeleteMCPServer(r.PathValue("name")); errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "сервер не найден")
		return
	} else if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	reply(w, map[string]bool{"ok": true})
}
