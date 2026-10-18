package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

// Команда проекта — постоянные агенты: у каждого роль, зона, модель и своя
// память, которая копится от задачи к задаче.
const teamSchema = `
CREATE TABLE IF NOT EXISTS team (
	id      TEXT PRIMARY KEY,
	project TEXT NOT NULL,
	data    TEXT NOT NULL,
	created INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS team_project ON team(project);
`

// TeamAgent — постоянный агент проекта.
type TeamAgent struct {
	ID       string    `json:"id"`
	Project  string    `json:"project"`
	Name     string    `json:"name"`
	Emoji    string    `json:"emoji"`
	Role     string    `json:"role"`     // инструкции роли: что делает, как, чего избегать
	Scope    []string  `json:"scope"`    // зона: папки, файлы, темы (web/, *.css, «оплата»)
	Model    string    `json:"model"`    // «провайдер/модель»
	Fallback string    `json:"fallback"` // резерв для Circuit Breaker
	Skills   []string  `json:"skills"`   // скиллы, которые агент получает всегда
	MCP      []string  `json:"mcp"`      // внешние MCP-серверы, которые агент получает всегда
	Enabled  bool      `json:"enabled"`
	Created  time.Time `json:"created"`
}

var agentIDRe = regexp.MustCompile(`^agent_[a-z0-9]{6,32}$`)

// Validate проверяет агента перед сохранением.
func (a TeamAgent) Validate() error {
	switch {
	case !agentIDRe.MatchString(a.ID):
		return errors.New("некорректный id агента")
	case strings.TrimSpace(a.Name) == "" || len([]rune(a.Name)) > 40:
		return errors.New("имя агента: от 1 до 40 символов")
	case strings.TrimSpace(a.Model) == "" || !strings.Contains(a.Model, "/"):
		return errors.New("выберите модель агента")
	case len([]rune(a.Role)) > 4000:
		return errors.New("инструкции роли длиннее 4000 символов")
	}
	return nil
}

func (s *Store) SaveTeamAgent(a TeamAgent) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if a.Created.IsZero() {
		a.Created = time.Now()
	}
	data, _ := json.Marshal(a)
	_, err := s.db.Exec(`INSERT INTO team(id, project, data, created) VALUES (?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET data = excluded.data`, a.ID, a.Project, string(data), a.Created.UnixMilli())
	return err
}

func (s *Store) TeamAgent(id string) (TeamAgent, error) {
	var data string
	err := s.db.QueryRow(`SELECT data FROM team WHERE id = ?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return TeamAgent{}, ErrNotFound
	} else if err != nil {
		return TeamAgent{}, err
	}
	var a TeamAgent
	return a, json.Unmarshal([]byte(data), &a)
}

// Team возвращает агентов проекта по порядку создания.
func (s *Store) Team(project string) ([]TeamAgent, error) {
	rows, err := s.db.Query(`SELECT data FROM team WHERE project = ? ORDER BY created`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TeamAgent{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var a TeamAgent
		if json.Unmarshal([]byte(data), &a) == nil {
			out = append(out, a)
		}
	}
	return out, rows.Err()
}

func (s *Store) DeleteTeamAgent(id string) error {
	res, err := s.db.Exec(`DELETE FROM team WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.db.Exec(`DELETE FROM project_memory WHERE agent = ?`, id)
	return nil
}
