package store

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// Виды git-хостингов.
const (
	GitHub = "github"
	GitLab = "gitlab"
	Gitea  = "gitea"
)

const gitSchema = `
CREATE TABLE IF NOT EXISTS git_accounts (
	id       INTEGER PRIMARY KEY,
	kind     TEXT NOT NULL,
	host     TEXT NOT NULL,
	label    TEXT NOT NULL DEFAULT '',
	username TEXT NOT NULL DEFAULT '',
	token    BLOB NOT NULL
);
`

// GitAccount — аккаунт на GitHub, GitLab или Gitea. Token расшифрован только
// при чтении через GitAccount(id); в списке он пустой.
type GitAccount struct {
	ID       int64  `json:"id"`
	Kind     string `json:"kind"`
	Host     string `json:"host"` // https://github.com, https://gitlab.example.com …
	Label    string `json:"label"`
	Username string `json:"username"`
	Token    string `json:"-"`
}

func (s *Store) AddGitAccount(a GitAccount) (int64, error) {
	sealed, err := s.vault.Seal([]byte(a.Token))
	if err != nil {
		return 0, err
	}
	res, err := s.db.Exec(`INSERT INTO git_accounts(kind, host, label, username, token) VALUES (?,?,?,?,?)`, a.Kind, a.Host, a.Label, a.Username, sealed)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) GitAccounts() ([]GitAccount, error) {
	rows, err := s.db.Query(`SELECT id, kind, host, label, username FROM git_accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GitAccount{}
	for rows.Next() {
		var a GitAccount
		if err := rows.Scan(&a.ID, &a.Kind, &a.Host, &a.Label, &a.Username); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GitAccount возвращает аккаунт с расшифрованным токеном.
func (s *Store) GitAccount(id int64) (GitAccount, error) {
	var a GitAccount
	var sealed []byte
	err := s.db.QueryRow(`SELECT id, kind, host, label, username, token FROM git_accounts WHERE id = ?`, id).Scan(&a.ID, &a.Kind, &a.Host, &a.Label, &a.Username, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return GitAccount{}, ErrNotFound
	} else if err != nil {
		return GitAccount{}, err
	}
	plain, err := s.vault.Open(sealed)
	if err != nil {
		return GitAccount{}, err
	}
	a.Token = string(plain)
	return a, nil
}

func (s *Store) DeleteGitAccount(id int64) error {
	res, err := s.db.Exec(`DELETE FROM git_accounts WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Режимы коммита результата агента.
const (
	CommitBranch = "branch" // новая ветка edy/…
	CommitDirect = "direct" // коммит в текущую ветку
	CommitNone   = "none"   // без коммита: изменения остаются в папке
)

// Что делать после коммита.
const (
	PushOff = "off"
	PushOn  = "push"
	PushPR  = "pr" // push и Pull Request (Merge Request в GitLab)
)

// ProjectSettings — настройки работы с репозиторием проекта.
type ProjectSettings struct {
	Commit      string `json:"commit"`
	Push        string `json:"push"`
	AccountID   int64  `json:"account_id"`
	AuthorName  string `json:"author_name"`
	AuthorEmail string `json:"author_email"`
	Prefix      string `json:"prefix"`
}

// DefaultProjectSettings — как было до настроек: новая ветка, без push.
func DefaultProjectSettings() ProjectSettings {
	return ProjectSettings{Commit: CommitBranch, Push: PushOff, Prefix: "Edy: "}
}

func (s *Store) ProjectSettings(path string) (ProjectSettings, error) {
	raw, err := s.Setting("project:" + path)
	if errors.Is(err, ErrNotFound) {
		return DefaultProjectSettings(), nil
	} else if err != nil {
		return ProjectSettings{}, err
	}
	ps := DefaultProjectSettings()
	if err := json.Unmarshal([]byte(raw), &ps); err != nil {
		return ProjectSettings{}, err
	}
	return ps, nil
}

func (s *Store) SetProjectSettings(path string, ps ProjectSettings) error {
	b, err := json.Marshal(ps)
	if err != nil {
		return err
	}
	return s.SetSetting("project:"+path, string(b))
}
