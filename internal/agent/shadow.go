package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Shadow — теневое версионирование: отдельный скрытый git-репозиторий в
// .edy/shadow.git, который хранит снимки проекта после каждого шага агента.
// Основной репозиторий проекта он не трогает.
type Shadow struct {
	Root string
}

// Checkpoint — один снимок.
type Checkpoint struct {
	ID      string    `json:"id"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

func (s Shadow) gitDir() string { return filepath.Join(s.Root, ".edy", "shadow.git") }

func (s Shadow) git(ctx context.Context, args ...string) (string, error) {
	// Снимок нельзя обрывать на полпути: убитый git оставляет index.lock и
	// неполные объекты, и откатиться потом нельзя. Поэтому остановка задачи
	// git не прерывает — команды короткие и доводятся до конца.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + s.gitDir(), "--work-tree=" + s.Root}, args...)...)
	cmd.Dir = s.Root
	// Снимки делает Edy, а не пользователь: своё имя автора и без подписи.
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Edy", "GIT_AUTHOR_EMAIL=edy@localhost", "GIT_COMMITTER_NAME=Edy", "GIT_COMMITTER_EMAIL=edy@localhost")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// Init создаёт теневой репозиторий или чинит недоделанный. Повторный
// `git init` для существующего репозитория безопасен, а исключения и
// настройки пишутся каждый раз: если создание когда-то оборвали, снимки
// не начнут захватывать саму папку .edy.
func (s Shadow) Init(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.gitDir()), 0o755); err != nil {
		return err
	}
	if _, err := s.git(ctx, "init", "-q"); err != nil {
		return err
	}
	// Служебные папки в снимки не попадают; .gitignore проекта учитывается сам.
	exclude := filepath.Join(s.gitDir(), "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(exclude), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(exclude, []byte(".edy/\n.git/\n"), 0o644); err != nil {
		return err
	}
	_, err := s.git(ctx, "config", "commit.gpgsign", "false")
	return err
}

// Snapshot сохраняет текущее состояние проекта и возвращает id снимка.
func (s Shadow) Snapshot(ctx context.Context, message string) (string, error) {
	if _, err := s.git(ctx, "add", "-A"); err != nil {
		return "", err
	}
	if _, err := s.git(ctx, "commit", "-q", "--allow-empty", "--no-verify", "-m", message); err != nil {
		return "", err
	}
	return s.git(ctx, "rev-parse", "HEAD")
}

// Restore возвращает проект к снимку: изменённые файлы восстанавливаются,
// созданные после снимка — удаляются. Файлы из .gitignore не трогаются.
func (s Shadow) Restore(ctx context.Context, id string) error {
	if id == "" || strings.ContainsAny(id, " -") {
		return errors.New("некорректный id снимка")
	}
	if _, err := s.git(ctx, "reset", "-q", "--hard", id); err != nil {
		return err
	}
	_, err := s.git(ctx, "clean", "-q", "-fd")
	return err
}

// List возвращает снимки, новые первыми.
func (s Shadow) List(ctx context.Context) ([]Checkpoint, error) {
	out, err := s.git(ctx, "log", "--format=%H%x1f%cI%x1f%s")
	if err != nil {
		return nil, err
	}
	var cps []Checkpoint
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\x1f")
		if len(parts) != 3 {
			continue
		}
		at, _ := time.Parse(time.RFC3339, parts[1])
		cps = append(cps, Checkpoint{ID: parts[0], At: at, Message: parts[2]})
	}
	return cps, nil
}

// Diff показывает изменения проекта относительно снимка (для отчёта).
func (s Shadow) Diff(ctx context.Context, from string) (string, error) {
	if _, err := s.git(ctx, "add", "-A"); err != nil {
		return "", err
	}
	return s.git(ctx, "diff", "--cached", "--stat", from)
}

// OpenShadow готовит теневой репозиторий проекта для правок вне задачи
// (например, через MCP): прячет .edy от основного git и создаёт shadow.git.
func OpenShadow(ctx context.Context, root string) (Shadow, error) {
	if isRepo(ctx, root) {
		if err := excludeEdyDir(root); err != nil {
			return Shadow{}, err
		}
	}
	s := Shadow{Root: root}
	return s, s.Init(ctx)
}

// Patch — полный текст изменений относительно снимка (для ревью), не длиннее limit байт.
func (s Shadow) Patch(ctx context.Context, from string, limit int) (string, error) {
	if _, err := s.git(ctx, "add", "-A"); err != nil {
		return "", err
	}
	out, err := s.git(ctx, "diff", "--cached", "--no-color", "-U3", from)
	if err != nil {
		return "", err
	}
	if len(out) > limit {
		out = out[:limit] + "\n… (изменения обрезаны: показаны первые " + fmt.Sprint(limit/1024) + " КБ)"
	}
	return out, nil
}

// RestoreFile возвращает один файл к снимку: изменённый — восстанавливается,
// созданный после снимка — удаляется.
func (s Shadow) RestoreFile(ctx context.Context, id, file string) error {
	if id == "" || strings.ContainsAny(id, " -") {
		return errors.New("некорректный id снимка")
	}
	if _, err := (Workspace{Root: s.Root}).resolve(file); err != nil {
		return err
	}
	if _, err := s.git(ctx, "cat-file", "-e", id+":"+file); err == nil {
		_, err := s.git(ctx, "checkout", id, "--", file)
		return err
	}
	// В снимке файла не было — его создала задача.
	if err := os.Remove(filepath.Join(s.Root, file)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
