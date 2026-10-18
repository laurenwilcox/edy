package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Виды подключения внешнего MCP-сервера.
const (
	MCPHTTP  = "http"  // удалённый сервер по Streamable HTTP
	MCPStdio = "stdio" // программа на этой машине (npx, uvx, бинарник)
)

const mcpSchema = `
CREATE TABLE IF NOT EXISTS mcp_servers (
	id          INTEGER PRIMARY KEY,
	name        TEXT NOT NULL UNIQUE,
	description TEXT NOT NULL DEFAULT '',
	enabled     INTEGER NOT NULL DEFAULT 1,
	config      BLOB NOT NULL
);
`

// MCPServer — внешний MCP-сервер из каталога Edy. Оркестратор выбирает
// нужные серверы под задачу, исполнитель получает их вместе с MCP Edy.
// Адрес, команда и особенно заголовки и переменные окружения (там бывают
// токены) хранятся зашифрованными.
type MCPServer struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	MCPConfig
}

// MCPConfig — как подключиться к серверу.
type MCPConfig struct {
	Type    string            `json:"type"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

var mcpNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Validate проверяет запись перед сохранением.
func (m MCPServer) Validate() error {
	if !mcpNameRe.MatchString(m.Name) {
		return errors.New("имя: латиница в нижнем регистре, цифры, «-» и «_», до 32 символов")
	}
	if m.Name == "edy" {
		return errors.New("имя edy занято встроенным MCP-сервером Edy")
	}
	switch m.Type {
	case MCPHTTP:
		if !strings.HasPrefix(m.URL, "https://") && !strings.HasPrefix(m.URL, "http://") {
			return errors.New("адрес сервера должен начинаться с https://")
		}
	case MCPStdio:
		if strings.TrimSpace(m.Command) == "" {
			return errors.New("укажите команду запуска, например npx")
		}
	default:
		return fmt.Errorf("тип %q: http или stdio", m.Type)
	}
	return nil
}

// SaveMCPServer добавляет сервер или заменяет сервер с тем же именем.
func (s *Store) SaveMCPServer(m MCPServer) error {
	if err := m.Validate(); err != nil {
		return err
	}
	raw, _ := json.Marshal(m.MCPConfig)
	sealed, err := s.vault.Seal(raw)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO mcp_servers(name, description, enabled, config) VALUES (?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET description = excluded.description, enabled = excluded.enabled, config = excluded.config`,
		m.Name, strings.TrimSpace(m.Description), m.Enabled, sealed)
	return err
}

// MCPServers возвращает каталог с расшифрованными настройками.
func (s *Store) MCPServers() ([]MCPServer, error) {
	rows, err := s.db.Query(`SELECT id, name, description, enabled, config FROM mcp_servers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MCPServer{}
	for rows.Next() {
		var m MCPServer
		var sealed []byte
		if err := rows.Scan(&m.ID, &m.Name, &m.Description, &m.Enabled, &sealed); err != nil {
			return nil, err
		}
		plain, err := s.vault.Open(sealed)
		if err != nil {
			return nil, fmt.Errorf("MCP-сервер %s: %w", m.Name, err)
		}
		if err := json.Unmarshal(plain, &m.MCPConfig); err != nil {
			return nil, fmt.Errorf("MCP-сервер %s: %w", m.Name, err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetMCPServerEnabled включает или выключает сервер, не трогая настройки.
func (s *Store) SetMCPServerEnabled(name string, enabled bool) error {
	res, err := s.db.Exec(`UPDATE mcp_servers SET enabled = ? WHERE name = ?`, enabled, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteMCPServer(name string) error {
	res, err := s.db.Exec(`DELETE FROM mcp_servers WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
