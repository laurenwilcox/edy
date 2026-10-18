package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Доска задач проекта (канбан). Задача проходит путь
// backlog → queued → running → done/needs_human/failed → approved/rejected/cancelled;
// каждое событие (шаги агента, смена модели, комментарии, ревью) пишется в её историю.
const boardSchema = `
CREATE TABLE IF NOT EXISTS board_tasks (
	id        TEXT PRIMARY KEY,
	project   TEXT NOT NULL,
	parent    TEXT NOT NULL DEFAULT '',
	title     TEXT NOT NULL DEFAULT '',
	data      TEXT NOT NULL,
	state     TEXT NOT NULL,
	position  INTEGER NOT NULL DEFAULT 0,
	created   INTEGER NOT NULL,
	updated   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS board_tasks_project ON board_tasks(project, state);
CREATE TABLE IF NOT EXISTS board_events (
	id      INTEGER PRIMARY KEY,
	task_id TEXT NOT NULL REFERENCES board_tasks(id) ON DELETE CASCADE,
	ts      INTEGER NOT NULL,
	kind    TEXT NOT NULL,
	text    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS board_events_task ON board_events(task_id, id);
`

// BoardTask — карточка задачи. Data — JSON с полями задачи (их набор
// принадлежит пакету tasks, в базе он хранится целиком).
type BoardTask struct {
	ID       string
	Project  string
	Parent   string
	Title    string
	Data     json.RawMessage
	State    string
	Position int64
	Created  time.Time
	Updated  time.Time
}

// BoardEvent — запись истории карточки.
type BoardEvent struct {
	ID   int64     `json:"id"`
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	Text string    `json:"text"`
}

// SaveBoardTask создаёт или обновляет карточку.
func (s *Store) SaveBoardTask(t BoardTask) error {
	if t.Created.IsZero() {
		t.Created = time.Now()
	}
	if t.Updated.IsZero() {
		t.Updated = time.Now()
	}
	_, err := s.db.Exec(`INSERT INTO board_tasks(id, project, parent, title, data, state, position, created, updated) VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET title = excluded.title, data = excluded.data, state = excluded.state,
			position = excluded.position, parent = excluded.parent, updated = excluded.updated`,
		t.ID, t.Project, t.Parent, t.Title, string(t.Data), t.State, t.Position, t.Created.UnixMilli(), t.Updated.UnixMilli())
	return err
}

func scanBoardTask(row interface{ Scan(...any) error }) (BoardTask, error) {
	var t BoardTask
	var data string
	var created, updated int64
	if err := row.Scan(&t.ID, &t.Project, &t.Parent, &t.Title, &data, &t.State, &t.Position, &created, &updated); err != nil {
		return BoardTask{}, err
	}
	t.Data = json.RawMessage(data)
	t.Created, t.Updated = time.UnixMilli(created), time.UnixMilli(updated)
	return t, nil
}

const boardCols = `id, project, parent, title, data, state, position, created, updated`

func (s *Store) BoardTask(id string) (BoardTask, error) {
	t, err := scanBoardTask(s.db.QueryRow(`SELECT `+boardCols+` FROM board_tasks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return BoardTask{}, ErrNotFound
	}
	return t, err
}

// BoardTasks возвращает карточки проекта (project пустой — все) по порядку.
func (s *Store) BoardTasks(project string) ([]BoardTask, error) {
	q := `SELECT ` + boardCols + ` FROM board_tasks`
	var args []any
	if project != "" {
		q += ` WHERE project = ?`
		args = append(args, project)
	}
	rows, err := s.db.Query(q+` ORDER BY position, created`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BoardTask{}
	for rows.Next() {
		t, err := scanBoardTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteBoardTask(id string) error {
	res, err := s.db.Exec(`DELETE FROM board_tasks WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) AddBoardEvent(taskID, kind, text string, at time.Time) error {
	_, err := s.db.Exec(`INSERT INTO board_events(task_id, ts, kind, text) VALUES (?,?,?,?)`, taskID, at.UnixMilli(), kind, text)
	return err
}

// BoardEvents возвращает историю карточки после события afterID.
func (s *Store) BoardEvents(taskID string, afterID int64) ([]BoardEvent, error) {
	rows, err := s.db.Query(`SELECT id, ts, kind, text FROM board_events WHERE task_id = ? AND id > ? ORDER BY id`, taskID, afterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BoardEvent{}
	for rows.Next() {
		var e BoardEvent
		var ts int64
		if err := rows.Scan(&e.ID, &ts, &e.Kind, &e.Text); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}
