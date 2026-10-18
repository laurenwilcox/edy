package tasks

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Копии проекта для параллельных агентов: каждый работает в своём
// git worktree внутри .edy/worktrees, основная папка проекта не меняется,
// пока вы не примете результат.

func gitC(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	// Коммиты подзадач делает Edy — без подписи и со своим автором.
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Edy", "GIT_AUTHOR_EMAIL=edy@localhost", "GIT_COMMITTER_NAME=Edy", "GIT_COMMITTER_EMAIL=edy@localhost")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()+" "+out.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func isGitRepo(dir string) bool {
	_, err := gitC(context.Background(), dir, "rev-parse", "--is-inside-work-tree")
	return err == nil
}

func epicDir(project, epicID string) string {
	return filepath.Join(project, ".edy", "worktrees", epicID)
}

// addWorktree создаёт копию проекта на новой ветке от commit.
func addWorktree(ctx context.Context, project, path, branch, commit string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	_, err := gitC(ctx, project, "worktree", "add", "-q", "-b", branch, path, commit)
	return err
}

// commitAll фиксирует всё сделанное в копии; false — изменений не было.
func commitAll(ctx context.Context, dir, message string) (bool, error) {
	if _, err := gitC(ctx, dir, "add", "-A"); err != nil {
		return false, err
	}
	if st, err := gitC(ctx, dir, "status", "--porcelain"); err != nil || st == "" {
		return false, err
	}
	_, err := gitC(ctx, dir, "-c", "commit.gpgsign=false", "commit", "-q", "--no-verify", "-m", message)
	return err == nil, err
}

// removeEpicWorktrees убирает все копии и ветки эпика.
func removeEpicWorktrees(project, epicID string, branches []string) {
	ctx := context.Background()
	dir := epicDir(project, epicID)
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			gitC(ctx, project, "worktree", "remove", "--force", filepath.Join(dir, e.Name()))
		}
	}
	os.RemoveAll(dir)
	gitC(ctx, project, "worktree", "prune")
	for _, b := range branches {
		if b != "" {
			gitC(ctx, project, "branch", "-D", b)
		}
	}
}
