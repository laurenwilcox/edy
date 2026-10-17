// Package gitops — работа с репозиторием проекта: коммит результата агента
// по настройкам проекта, push/pull/clone с токеном аккаунта и создание
// Pull Request (GitHub, Gitea) или Merge Request (GitLab).
package gitops

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/laurenwilcox/name/internal/store"
)

// IsRepo сообщает, является ли папка git-репозиторием.
func IsRepo(root string) bool {
	_, err := git(context.Background(), root, nil, "rev-parse", "--is-inside-work-tree")
	return err == nil
}

func git(ctx context.Context, root string, extra []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append(extra, args...)...)
	cmd.Dir = root
	// Git не должен спрашивать пароль в терминале — только токен из аккаунта.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// authArgs передаёт токен заголовком только на время команды: он не
// записывается ни в URL удалённого репозитория, ни в .git/config.
func authArgs(a *store.GitAccount) []string {
	if a == nil || a.Token == "" {
		return nil
	}
	user := a.Username
	if user == "" {
		switch a.Kind {
		case store.GitHub:
			user = "x-access-token"
		case store.GitLab:
			user = "oauth2"
		default:
			user = "edy"
		}
	}
	basic := base64.StdEncoding.EncodeToString([]byte(user + ":" + a.Token))
	return []string{"-c", "http.extraHeader=Authorization: Basic " + basic}
}

// Info — состояние репозитория для вкладки «Git».
type Info struct {
	IsRepo  bool     `json:"is_repo"`
	Branch  string   `json:"branch"`
	Remote  string   `json:"remote"`
	Changed []string `json:"changed"` // строки git status --short
	Log     []string `json:"log"`     // последние коммиты
}

func GetInfo(ctx context.Context, root string) Info {
	if !IsRepo(root) {
		return Info{Changed: []string{}, Log: []string{}}
	}
	info := Info{IsRepo: true, Changed: []string{}, Log: []string{}}
	info.Branch, _ = git(ctx, root, nil, "rev-parse", "--abbrev-ref", "HEAD")
	info.Remote, _ = git(ctx, root, nil, "remote", "get-url", "origin")
	if st, err := git(ctx, root, nil, "status", "--short"); err == nil && st != "" {
		info.Changed = strings.Split(st, "\n")
	}
	if lg, err := git(ctx, root, nil, "log", "-8", "--format=%h %s (%cr)"); err == nil && lg != "" {
		info.Log = strings.Split(lg, "\n")
	}
	info.Remote = stripCredentials(info.Remote)
	return info
}

// stripCredentials убирает логин и пароль из URL, если они там есть.
func stripCredentials(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}

// Result — что сделано с результатом агента.
type Result struct {
	Committed bool   `json:"committed"`
	Branch    string `json:"branch,omitempty"`
	Pushed    bool   `json:"pushed"`
	PRURL     string `json:"pr_url,omitempty"`
	Note      string `json:"note"`
}

// Finalize оформляет изменения в проекте по его настройкам: коммит в новую
// ветку или в текущую (или без коммита), затем push и Pull/Merge Request.
// newBranch — имя ветки для режима CommitBranch.
func Finalize(ctx context.Context, root string, ps store.ProjectSettings, acc *store.GitAccount, newBranch, title, body string) (Result, error) {
	if !IsRepo(root) {
		return Result{Note: "Папка без git: изменения оставлены в ней, снимки доступны в разделе «Проект»."}, nil
	}
	if ps.Commit == store.CommitNone {
		return Result{Note: "Коммит отключён в настройках: изменения оставлены в рабочей копии."}, nil
	}
	base, err := git(ctx, root, nil, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return Result{}, err
	}
	branch := base
	if ps.Commit == store.CommitBranch {
		if _, err := git(ctx, root, nil, "check-ref-format", "--branch", newBranch); err != nil {
			return Result{}, fmt.Errorf("некорректное имя ветки %q", newBranch)
		}
		if _, err := git(ctx, root, nil, "checkout", "-q", "-b", newBranch); err != nil {
			return Result{}, err
		}
		branch = newBranch
	}
	if _, err := git(ctx, root, nil, "add", "-A"); err != nil {
		return Result{}, err
	}
	msg := ps.Prefix + title
	if body != "" {
		msg += "\n\n" + body
	}
	args := []string{"commit", "-q", "-m", msg}
	if ps.AuthorName != "" && ps.AuthorEmail != "" {
		// Свой автор коммитов, например «Edy Bot»; коммиттером остаётся пользователь.
		args = append(args, "--author="+ps.AuthorName+" <"+ps.AuthorEmail+">")
	}
	if _, err := git(ctx, root, nil, args...); err != nil {
		return Result{}, err
	}
	res := Result{Committed: true, Branch: branch, Note: "Коммит в ветке " + branch}
	if ps.Commit == store.CommitBranch {
		res.Note += " (основная ветка " + base + " не изменилась)"
	}

	if ps.Push == store.PushOff || ps.Push == "" {
		return res, nil
	}
	if err := Push(ctx, root, branch, acc); err != nil {
		res.Note += ". Push не удался: " + err.Error()
		return res, nil
	}
	res.Pushed = true
	res.Note += ", отправлено в origin"
	if ps.Push == store.PushPR && ps.Commit == store.CommitBranch {
		if acc == nil {
			res.Note += ". Pull Request не создан: в настройках проекта не выбран аккаунт"
			return res, nil
		}
		remote, _ := git(ctx, root, nil, "remote", "get-url", "origin")
		prURL, err := CreatePR(ctx, *acc, remote, base, branch, ps.Prefix+title, body)
		if err != nil {
			res.Note += ". Pull Request не создан: " + err.Error()
			return res, nil
		}
		res.PRURL = prURL
		res.Note += ", открыт запрос на слияние"
	}
	return res, nil
}

// Push отправляет ветку в origin.
func Push(ctx context.Context, root, branch string, acc *store.GitAccount) error {
	if branch == "" {
		var err error
		if branch, err = git(ctx, root, nil, "rev-parse", "--abbrev-ref", "HEAD"); err != nil {
			return err
		}
	}
	_, err := git(ctx, root, authArgs(acc), "push", "-u", "origin", branch)
	return err
}

// Pull забирает изменения текущей ветки из origin (только fast-forward).
func Pull(ctx context.Context, root string, acc *store.GitAccount) (string, error) {
	return git(ctx, root, authArgs(acc), "pull", "--ff-only")
}

// Clone клонирует репозиторий в dest.
func Clone(ctx context.Context, repoURL, dest string, acc *store.GitAccount) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("папка %s уже существует", dest)
	}
	_, err := git(ctx, filepath.Dir(dest), authArgs(acc), "clone", "--", stripCredentials(repoURL), dest)
	return err
}

var scpLike = regexp.MustCompile(`^[\w.-]+@([\w.-]+):(.+)$`)

// RepoPath достаёт «владелец/репозиторий» из URL удалённого репозитория
// (https://host/owner/repo.git или git@host:owner/repo.git).
func RepoPath(remote string) (string, error) {
	var path string
	if m := scpLike.FindStringSubmatch(remote); m != nil {
		path = m[2]
	} else {
		u, err := url.Parse(remote)
		if err != nil || (u.Host == "" && u.Scheme != "file") {
			return "", fmt.Errorf("не удалось разобрать адрес репозитория %q", remote)
		}
		path = u.Path
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if !strings.Contains(path, "/") {
		return "", fmt.Errorf("в адресе %q нет владельца и имени репозитория", remote)
	}
	return path, nil
}

// RepoName — имя папки для клона.
func RepoName(remote string) (string, error) {
	p, err := RepoPath(remote)
	if err != nil {
		return "", err
	}
	return p[strings.LastIndex(p, "/")+1:], nil
}
