package store

import (
	"time"
)

// Память проекта: короткие заметки, которые помогают следующим агентам —
// как запускать тесты, что хрупко, чем кончились прошлые задачи.
const memorySchema = `
CREATE TABLE IF NOT EXISTS project_memory (
	id      INTEGER PRIMARY KEY,
	project TEXT NOT NULL,
	kind    TEXT NOT NULL,
	text    TEXT NOT NULL,
	source  TEXT NOT NULL DEFAULT '',
	pinned  INTEGER NOT NULL DEFAULT 0,
	created INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS project_memory_project ON project_memory(project, id);
`

// Виды заметок.
const (
	NoteFact   = "fact"   // факт о проекте (как собрать, где что лежит)
	NoteLesson = "lesson" // урок: что не получилось и почему
	NoteTask   = "task"   // итог принятой задачи
	NoteNote   = "note"   // заметка пользователя
)

// Note — заметка памяти проекта.
type Note struct {
	ID      int64     `json:"id"`
	Project string    `json:"project"`
	Kind    string    `json:"kind"`
	Text    string    `json:"text"`
	Source  string    `json:"source"`
	Pinned  bool      `json:"pinned"`
	Created time.Time `json:"created"`
}

func (s *Store) AddNote(n Note) (int64, error) {
	if n.Created.IsZero() {
		n.Created = time.Now()
	}
	res, err := s.db.Exec(`INSERT INTO project_memory(project, kind, text, source, pinned, created) VALUES (?,?,?,?,?,?)`,
		n.Project, n.Kind, n.Text, n.Source, n.Pinned, n.Created.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Notes возвращает заметки проекта, новые первыми.
func (s *Store) Notes(project string) ([]Note, error) {
	rows, err := s.db.Query(`SELECT id, project, kind, text, source, pinned, created FROM project_memory WHERE project = ? ORDER BY id DESC`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Note{}
	for rows.Next() {
		var n Note
		var created int64
		if err := rows.Scan(&n.ID, &n.Project, &n.Kind, &n.Text, &n.Source, &n.Pinned, &created); err != nil {
			return nil, err
		}
		n.Created = time.UnixMilli(created)
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) SetNotePinned(project string, id int64, pinned bool) error {
	res, err := s.db.Exec(`UPDATE project_memory SET pinned = ? WHERE id = ? AND project = ?`, pinned, id, project)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteNote(project string, id int64) error {
	res, err := s.db.Exec(`DELETE FROM project_memory WHERE id = ? AND project = ?`, id, project)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
