package store

import (
	"encoding/binary"
	"math"
	"time"
)

// Векторы смыслового поиска: кусок кода или заметка памяти → эмбеддинг.
const embeddingsSchema = `
CREATE TABLE IF NOT EXISTS embeddings (
	project TEXT NOT NULL,
	key     TEXT NOT NULL,
	hash    TEXT NOT NULL,
	model   TEXT NOT NULL,
	vec     BLOB NOT NULL,
	updated INTEGER NOT NULL,
	PRIMARY KEY (project, key)
);
`

// Embedding — сохранённый вектор (нормированный, float32).
type Embedding struct {
	Key   string
	Hash  string
	Model string
	Vec   []float32
}

func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

func decodeVec(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

// Embeddings возвращает векторы проекта для модели.
func (s *Store) Embeddings(project, model string) ([]Embedding, error) {
	rows, err := s.db.Query(`SELECT key, hash, model, vec FROM embeddings WHERE project = ? AND model = ?`, project, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Embedding
	for rows.Next() {
		var e Embedding
		var b []byte
		if err := rows.Scan(&e.Key, &e.Hash, &e.Model, &b); err != nil {
			return nil, err
		}
		e.Vec = decodeVec(b)
		out = append(out, e)
	}
	return out, rows.Err()
}

// SaveEmbeddings сохраняет векторы и удаляет ключи, которых больше нет
// (keep — все актуальные ключи проекта); векторы другой модели удаляются.
func (s *Store) SaveEmbeddings(project, model string, list []Embedding, keep map[string]bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	for _, e := range list {
		if _, err := tx.Exec(`INSERT INTO embeddings(project, key, hash, model, vec, updated) VALUES (?,?,?,?,?,?)
			ON CONFLICT(project, key) DO UPDATE SET hash = excluded.hash, model = excluded.model, vec = excluded.vec, updated = excluded.updated`,
			project, e.Key, e.Hash, model, encodeVec(e.Vec), now); err != nil {
			return err
		}
	}
	if keep != nil {
		rows, err := tx.Query(`SELECT key, model FROM embeddings WHERE project = ?`, project)
		if err != nil {
			return err
		}
		var stale []string
		for rows.Next() {
			var k, m string
			rows.Scan(&k, &m)
			if !keep[k] || m != model {
				stale = append(stale, k)
			}
		}
		rows.Close()
		for _, k := range stale {
			if _, err := tx.Exec(`DELETE FROM embeddings WHERE project = ? AND key = ?`, project, k); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
