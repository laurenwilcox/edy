package omniroute

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// MCPHeader подключает к CLI-утилите MCP-сервер Edy: значение — адрес /mcp
// этого же Edy (только localhost). Так Оркестратор даёт агенту инструменты
// Edy: утилита правит проект через них, а Edy снимает снимки и ведёт журнал.
const MCPHeader = "X-Edy-MCP"

// MCPAttach — MCP-сервер, который нужно подключить к утилите.
type MCPAttach struct {
	URL   string
	Token string
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
	cfg := map[string]any{"mcpServers": map[string]any{"edy": map[string]any{
		"type":    "http",
		"url":     a.URL,
		"headers": map[string]string{"Authorization": "Bearer " + a.Token},
	}}}
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
	args := []string{
		"--mcp-config", f.Name(), "--strict-mcp-config",
		"--allowedTools", "mcp__edy",
		"--disallowedTools", "Bash", "Edit", "Write", "MultiEdit", "NotebookEdit",
	}
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
