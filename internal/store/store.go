// Package store — локальная база Edy на SQLite (чистый Go, без CGO).
// Секреты попадают в базу только в зашифрованном виде.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/laurenwilcox/name/internal/vault"
	_ "modernc.org/sqlite"
)

// Типы провайдеров, которые понимает API-адаптер OmniRoute.
const (
	KindOpenAI    = "openai"    // любой OpenAI-совместимый API: OpenAI, OpenRouter, DeepSeek, Ollama…
	KindAnthropic = "anthropic" // Anthropic Messages API
	KindCLI       = "cli"       // терминальная утилита в PTY: Ollama, Claude Code, Aider…
)

var ErrNotFound = errors.New("store: не найдено")

const schema = `
CREATE TABLE IF NOT EXISTS providers (
	id        INTEGER PRIMARY KEY,
	name      TEXT NOT NULL UNIQUE,
	kind      TEXT NOT NULL,
	base_url  TEXT NOT NULL DEFAULT '',
	models    TEXT NOT NULL DEFAULT '',
	price_in  REAL NOT NULL DEFAULT 0,
	price_out REAL NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS api_keys (
	id          INTEGER PRIMARY KEY,
	provider_id INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
	label       TEXT NOT NULL DEFAULT '',
	secret      BLOB NOT NULL,
	disabled    INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS usage (
	id            INTEGER PRIMARY KEY,
	ts            INTEGER NOT NULL,
	provider      TEXT NOT NULL,
	model         TEXT NOT NULL,
	client        TEXT NOT NULL DEFAULT '',
	input_tokens  INTEGER NOT NULL,
	output_tokens INTEGER NOT NULL,
	cost_usd      REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS usage_ts ON usage(ts);
`

// migrations добавляют колонки, появившиеся после первой версии схемы.
var migrations = []string{
	`ALTER TABLE providers ADD COLUMN command TEXT NOT NULL DEFAULT '[]'`,
}

// Provider — источник ИИ, подключённый к OmniRoute.
type Provider struct {
	ID       int64
	Name     string // используется как префикс модели: "<name>/<model>"
	Kind     string
	BaseURL  string
	Models   []string
	PriceIn  float64 // USD за 1M входных токенов
	PriceOut float64 // USD за 1M выходных токенов
	// Command — аргументы запуска для KindCLI с подстановками {model} и {prompt}.
	Command []string
}

// Cost считает стоимость запроса в долларах.
func (p Provider) Cost(in, out int64) float64 {
	return (float64(in)*p.PriceIn + float64(out)*p.PriceOut) / 1e6
}

// Key — расшифрованный API-ключ.
type Key struct {
	ID     int64
	Label  string
	Secret string
}

// Usage — одна запись расхода токенов.
type Usage struct {
	Provider     string
	Model        string
	Client       string
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
}

type Store struct {
	db    *sql.DB
	vault *vault.Vault
}

// Open открывает (или создаёт) базу по пути path.
func Open(path string, v *vault.Vault) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: миграция схемы: %w", err)
	}
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("store: миграция схемы: %w", err)
		}
	}
	return &Store{db: db, vault: v}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) AddProvider(p Provider) (int64, error) {
	command, err := json.Marshal(p.Command)
	if err != nil {
		return 0, err
	}
	res, err := s.db.Exec(`INSERT INTO providers(name, kind, base_url, models, price_in, price_out, command) VALUES (?,?,?,?,?,?,?)`,
		p.Name, p.Kind, p.BaseURL, strings.Join(p.Models, ","), p.PriceIn, p.PriceOut, command)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) Provider(name string) (Provider, error) {
	row := s.db.QueryRow(`SELECT id, name, kind, base_url, models, price_in, price_out, command FROM providers WHERE name = ?`, name)
	p, err := scanProvider(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Provider{}, ErrNotFound
	}
	return p, err
}

func (s *Store) Providers() ([]Provider, error) {
	rows, err := s.db.Query(`SELECT id, name, kind, base_url, models, price_in, price_out, command FROM providers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Provider
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func scanProvider(row interface{ Scan(...any) error }) (Provider, error) {
	var p Provider
	var models, command string
	if err := row.Scan(&p.ID, &p.Name, &p.Kind, &p.BaseURL, &models, &p.PriceIn, &p.PriceOut, &command); err != nil {
		return Provider{}, err
	}
	if err := json.Unmarshal([]byte(command), &p.Command); err != nil {
		return Provider{}, fmt.Errorf("store: повреждённая команда провайдера %q: %w", p.Name, err)
	}
	for _, m := range strings.Split(models, ",") {
		if m = strings.TrimSpace(m); m != "" {
			p.Models = append(p.Models, m)
		}
	}
	return p, nil
}

// AddKey шифрует и сохраняет API-ключ провайдера.
func (s *Store) AddKey(providerID int64, label, secret string) (int64, error) {
	sealed, err := s.vault.Seal([]byte(secret))
	if err != nil {
		return 0, err
	}
	res, err := s.db.Exec(`INSERT INTO api_keys(provider_id, label, secret) VALUES (?,?,?)`, providerID, label, sealed)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Keys возвращает активные ключи провайдера в порядке добавления.
func (s *Store) Keys(providerID int64) ([]Key, error) {
	rows, err := s.db.Query(`SELECT id, label, secret FROM api_keys WHERE provider_id = ? AND disabled = 0 ORDER BY id`, providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		var k Key
		var sealed []byte
		if err := rows.Scan(&k.ID, &k.Label, &sealed); err != nil {
			return nil, err
		}
		plain, err := s.vault.Open(sealed)
		if err != nil {
			return nil, fmt.Errorf("store: не удалось расшифровать ключ %d: %w", k.ID, err)
		}
		k.Secret = string(plain)
		out = append(out, k)
	}
	return out, rows.Err()
}

// DisableKey выключает ключ (например, после 401 от провайдера).
func (s *Store) DisableKey(id int64) error {
	_, err := s.db.Exec(`UPDATE api_keys SET disabled = 1 WHERE id = ?`, id)
	return err
}

func (s *Store) Setting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key, value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (s *Store) RecordUsage(u Usage) error {
	_, err := s.db.Exec(`INSERT INTO usage(ts, provider, model, client, input_tokens, output_tokens, cost_usd) VALUES (?,?,?,?,?,?,?)`,
		time.Now().Unix(), u.Provider, u.Model, u.Client, u.InputTokens, u.OutputTokens, u.CostUSD)
	return err
}

// SpentSince возвращает сумму расходов в долларах начиная с момента since.
func (s *Store) SpentSince(since time.Time) (float64, error) {
	var total float64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(cost_usd), 0) FROM usage WHERE ts >= ?`, since.Unix()).Scan(&total)
	return total, err
}
