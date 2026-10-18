package omniroute

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/laurenwilcox/name/internal/store"
)

// MCPHeader подключает к CLI-утилите MCP-сервер Edy: значение — адрес /mcp
// этого же Edy (только localhost). Так Оркестратор даёт агенту инструменты
// Edy: утилита правит проект через них, а Edy снимает снимки и ведёт журнал.
const MCPHeader = "X-Edy-MCP"

// MCPServersHeader — имена внешних MCP-серверов из каталога Edy через
// запятую, которые утилита получает вместе с MCP Edy. Их настройки (адреса,
// токены) берутся из каталога: в заголовке только имена.
const MCPServersHeader = "X-Edy-MCP-Servers"

// MCPAttach — MCP-сервер Edy и выданные вместе с ним внешние серверы.
type MCPAttach struct {
	URL   string
	Token string
	Extra []store.MCPServer
}

// mcpFlags — как подключить MCP к конкретной утилите. Флаги вставляются
// сразу после имени программы, до остальных аргументов.
type mcpFlags func(a MCPAttach) (args []string, cleanup func(), err error)

// Утилиты, которые умеют брать MCP-сервер из аргументов. У остальных MCP
// настраивается в их собственных файлах, поэтому Edy работает с ними через
// эмуляцию инструментов в промпте.
var mcpCapable = map[string]mcpFlags{
	"claude": claudeMCP,
}

// SupportsMCP сообщает, умеет ли утилита из команды провайдера подключать MCP Edy.
func SupportsMCP(command []string) bool {
	if len(command) == 0 {
		return false
	}
	_, ok := mcpCapable[filepath.Base(command[0])]
	return ok
}

// claudeMCP: Claude Code получает конфиг из файла (--mcp-config), только
// сервер Edy (--strict-mcp-config) и разрешение на его инструменты. Свои
// инструменты правки файлов и терминал отключены: все изменения идут через
// Edy, а тесты запускает run_checks.
func claudeMCP(a MCPAttach) ([]string, func(), error) {
	servers := map[string]any{"edy": map[string]any{
		"type":    "http",
		"url":     a.URL,
		"headers": map[string]string{"Authorization": "Bearer " + a.Token},
	}}
	allowed := []string{"mcp__edy"}
	for _, x := range a.Extra {
		switch x.Type {
		case store.MCPHTTP:
			servers[x.Name] = map[string]any{"type": "http", "url": x.URL, "headers": x.Headers}
		case store.MCPStdio:
			servers[x.Name] = map[string]any{"type": "stdio", "command": x.Command, "args": x.Args, "env": x.Env}
		default:
			continue
		}
		allowed = append(allowed, "mcp__"+x.Name)
	}
	cfg := map[string]any{"mcpServers": servers}
	data, _ := json.Marshal(cfg)
	f, err := os.CreateTemp("", "edy-mcp-*.json")
	if err != nil {
		return nil, nil, err
	}
	// В файле токен Edy: он доступен только владельцу и удаляется после запуска.
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, nil, err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, nil, err
	}
	f.Close()
	args := append([]string{"--mcp-config", f.Name(), "--strict-mcp-config", "--allowedTools"}, allowed...)
	args = append(args, "--disallowedTools", "Bash", "Edit", "Write", "MultiEdit", "NotebookEdit")
	return args, func() { os.Remove(f.Name()) }, nil
}

// checkMCPURL пропускает только адрес /mcp на этой машине: токен Edy не
// должен уходить на сторонний сервер.
func checkMCPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Path != "/mcp" {
		return fmt.Errorf("%s: ожидается http://127.0.0.1:<порт>/mcp?…", MCPHeader)
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
		return nil
	}
	return fmt.Errorf("%s: MCP-сервер должен быть на этой машине", MCPHeader)
}

// extraMCP находит в каталоге включённые серверы с именами из заголовка.
func (s *Server) extraMCP(header string) ([]store.MCPServer, error) {
	if strings.TrimSpace(header) == "" {
		return nil, nil
	}
	all, err := s.store.MCPServers()
	if err != nil {
		return nil, err
	}
	byName := map[string]store.MCPServer{}
	for _, m := range all {
		if m.Enabled {
			byName[m.Name] = m
		}
	}
	var out []store.MCPServer
	for _, name := range strings.Split(header, ",") {
		name = strings.TrimSpace(name)
		m, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("MCP-сервер %q не найден в каталоге или выключен", name)
		}
		out = append(out, m)
	}
	return out, nil
}
