package store

import (
	"database/sql"
	"errors"
	"sort"
	"time"
)

// Роли агентов.
const (
	RoleOrchestrator = "orchestrator"
	RoleExecutor     = "executor"
	RoleMCP          = "mcp" // внешний агент (Claude Code, Gemini CLI…), подключённый по MCP
	RoleReviewer     = "reviewer"
	RoleMicro        = "micro" // разовая задача со строгим JSON-ответом
)

// Состояния агентов.
const (
	AgentRunning     = "running"
	AgentDone        = "done"
	AgentFailed      = "failed"
	AgentCancelled   = "cancelled"
	AgentInterrupted = "interrupted" // Edy закрыли, пока агент работал
)

const agentsSchema = `
CREATE TABLE IF NOT EXISTS agents (
	id       TEXT PRIMARY KEY,
	role     TEXT NOT NULL,
	model    TEXT NOT NULL,
	project  TEXT NOT NULL DEFAULT '',
	task_id  TEXT NOT NULL DEFAULT '',
	goal     TEXT NOT NULL DEFAULT '',
	state    TEXT NOT NULL,
	summary  TEXT NOT NULL DEFAULT '',
	error    TEXT NOT NULL DEFAULT '',
	created  INTEGER NOT NULL,
	finished INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS agents_created ON agents(created);
CREATE TABLE IF NOT EXISTS agent_events (
	id       INTEGER PRIMARY KEY,
	agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
	ts       INTEGER NOT NULL,
	kind     TEXT NOT NULL,
	text     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS agent_events_agent ON agent_events(agent_id, id);
`

// Agent — запись журнала агентов.
type Agent struct {
	ID       string    `json:"id"`
	Role     string    `json:"role"`
	Model    string    `json:"model"`
	Project  string    `json:"project"`
	TaskID   string    `json:"task_id"`
	Goal     string    `json:"goal"`
	State    string    `json:"state"`
	Summary  string    `json:"summary"`
	Error    string    `json:"error"`
	Created  time.Time `json:"created"`
	Finished time.Time `json:"finished,omitzero"`
	Events   int       `json:"events"`
	// Last — последнее действие агента (для дашборда «кто что делает»).
	Last string `json:"last,omitempty"`
}

// AgentEvent — одно действие агента.
type AgentEvent struct {
	ID   int64     `json:"id"`
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	Text string    `json:"text"`
}

func (s *Store) CreateAgent(a Agent) error {
	if a.Created.IsZero() {
		a.Created = time.Now()
	}
	if a.State == "" {
		a.State = AgentRunning
	}
	_, err := s.db.Exec(`INSERT INTO agents(id, role, model, project, task_id, goal, state, created) VALUES (?,?,?,?,?,?,?,?)`,
		a.ID, a.Role, a.Model, a.Project, a.TaskID, a.Goal, a.State, a.Created.UnixMilli())
	return err
}

// FinishAgent записывает итог работы агента.
func (s *Store) FinishAgent(id, state, summary, errText string) error {
	_, err := s.db.Exec(`UPDATE agents SET state = ?, summary = ?, error = ?, finished = ? WHERE id = ?`,
		state, summary, errText, time.Now().UnixMilli(), id)
	return err
}

// TouchAgent обновляет состояние агента, который работает урывками (Оркестратор
// отвечает на реплики): время окончания ставится, когда он ответил, и
// сбрасывается, когда снова начал работать.
func (s *Store) TouchAgent(id, state, summary string) error {
	finished := time.Now().UnixMilli()
	if state == AgentRunning {
		finished = 0
	}
	_, err := s.db.Exec(`UPDATE agents SET state = ?, summary = ?, finished = ? WHERE id = ?`, state, summary, finished, id)
	return err
}

func (s *Store) AddAgentEvent(agentID, kind, text string, at time.Time) error {
	_, err := s.db.Exec(`INSERT INTO agent_events(agent_id, ts, kind, text) VALUES (?,?,?,?)`, agentID, at.UnixMilli(), kind, text)
	return err
}

// InterruptRunningAgents помечает агентов, оставшихся «работающими» с прошлого запуска.
func (s *Store) InterruptRunningAgents() (int64, error) {
	res, err := s.db.Exec(`UPDATE agents SET state = ?, error = 'Edy был закрыт во время работы', finished = ? WHERE state = ?`,
		AgentInterrupted, time.Now().UnixMilli(), AgentRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const agentCols = `a.id, a.role, a.model, a.project, a.task_id, a.goal, a.state, a.summary, a.error, a.created, a.finished,
	(SELECT COUNT(*) FROM agent_events e WHERE e.agent_id = a.id),
	COALESCE((SELECT substr(e.text, 1, 300) FROM agent_events e WHERE e.agent_id = a.id ORDER BY e.id DESC LIMIT 1), '')`

func scanAgent(row interface{ Scan(...any) error }) (Agent, error) {
	var a Agent
	var created, finished int64
	if err := row.Scan(&a.ID, &a.Role, &a.Model, &a.Project, &a.TaskID, &a.Goal, &a.State, &a.Summary, &a.Error, &created, &finished, &a.Events, &a.Last); err != nil {
		return Agent{}, err
	}
	a.Created = time.UnixMilli(created)
	if finished > 0 {
		a.Finished = time.UnixMilli(finished)
	}
	return a, nil
}

// Agents возвращает последних агентов, новые первыми.
func (s *Store) Agents(limit int) ([]Agent, error) {
	rows, err := s.db.Query(`SELECT `+agentCols+` FROM agents a ORDER BY a.created DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Agent{}
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) Agent(id string) (Agent, error) {
	a, err := scanAgent(s.db.QueryRow(`SELECT `+agentCols+` FROM agents a WHERE a.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, ErrNotFound
	}
	return a, err
}

// AgentEvents возвращает действия агента после события afterID.
func (s *Store) AgentEvents(agentID string, afterID int64) ([]AgentEvent, error) {
	rows, err := s.db.Query(`SELECT id, ts, kind, text FROM agent_events WHERE agent_id = ? AND id > ? ORDER BY id`, agentID, afterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentEvent{}
	for rows.Next() {
		var e AgentEvent
		var ts int64
		if err := rows.Scan(&e.ID, &ts, &e.Kind, &e.Text); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ModelStat — как модель справляется с задачами и сколько стоит.
type ModelStat struct {
	Model     string `json:"model"`     // «провайдер/модель»
	Attempts  int64  `json:"attempts"`  // сколько раз брала задачу исполнителем
	Successes int64  `json:"successes"` // сколько раз прошла проверки
	// Refused — сколько раз отказалась работать (CLI без прав, запрос больше лимита).
	Refused int64   `json:"refused"`
	CostUSD float64 `json:"cost_usd"` // расходы модели за период (все роли)
	Tokens  int64   `json:"tokens"`
}

// ModelStats собирает успехи исполнителей из журнала агентов и расходы из
// учёта токенов с момента since.
func (s *Store) ModelStats(since time.Time) ([]ModelStat, error) {
	stats := map[string]*ModelStat{}
	get := func(m string) *ModelStat {
		if stats[m] == nil {
			stats[m] = &ModelStat{Model: m}
		}
		return stats[m]
	}
	rows, err := s.db.Query(`SELECT model, COUNT(*), SUM(CASE WHEN state = ? THEN 1 ELSE 0 END),
		SUM(CASE WHEN error LIKE 'отказалась работать%' THEN 1 ELSE 0 END) FROM agents
		WHERE role = ? AND created >= ? AND state != ? GROUP BY model`, AgentDone, RoleExecutor, since.UnixMilli(), AgentRunning)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m string
		var a, ok, refused int64
		if err := rows.Scan(&m, &a, &ok, &refused); err != nil {
			rows.Close()
			return nil, err
		}
		st := get(m)
		st.Attempts, st.Successes, st.Refused = a, ok, refused
	}
	rows.Close()
	rows, err = s.db.Query(`SELECT provider || '/' || model, SUM(cost_usd), SUM(input_tokens + output_tokens) FROM usage WHERE ts >= ? GROUP BY provider, model`, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m string
		var cost float64
		var tok int64
		if err := rows.Scan(&m, &cost, &tok); err != nil {
			return nil, err
		}
		st := get(m)
		st.CostUSD, st.Tokens = cost, tok
	}
	out := make([]ModelStat, 0, len(stats))
	for _, st := range stats {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Attempts != out[j].Attempts {
			return out[i].Attempts > out[j].Attempts
		}
		return out[i].Model < out[j].Model
	})
	return out, rows.Err()
}
