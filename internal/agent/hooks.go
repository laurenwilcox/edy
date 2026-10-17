package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const hookTimeout = 10 * time.Minute

// Hook — проверка, которую Edy запускает сам, без ИИ.
type Hook struct {
	Name string   `json:"name"`
	Cmd  []string `json:"cmd"`
}

// HooksFile — свои хуки проекта; если файла нет, хуки определяются по стеку.
const HooksFile = ".edy/hooks.json"

// DetectHooks возвращает хуки проекта: из .edy/hooks.json или по стеку
// (Go — go vet и go test; Python — ruff и pytest, если они установлены).
func DetectHooks(root string) ([]Hook, error) {
	if data, err := os.ReadFile(filepath.Join(root, HooksFile)); err == nil {
		var hooks []Hook
		if err := json.Unmarshal(data, &hooks); err != nil {
			return nil, fmt.Errorf("%s: %w", HooksFile, err)
		}
		return hooks, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	var hooks []Hook
	if exists(root, "go.mod") {
		hooks = append(hooks, Hook{"go vet", []string{"go", "vet", "./..."}}, Hook{"go test", []string{"go", "test", "./..."}})
	}
	if exists(root, "pyproject.toml") || exists(root, "requirements.txt") || exists(root, "setup.py") || hasPython(root) {
		if _, err := exec.LookPath("ruff"); err == nil {
			hooks = append(hooks, Hook{"ruff", []string{"ruff", "check", "."}})
		}
		if _, err := exec.LookPath("pytest"); err == nil && hasPythonTests(root) {
			hooks = append(hooks, Hook{"pytest", []string{"pytest", "-q"}})
		}
	}
	return hooks, nil
}

func exists(root, name string) bool {
	_, err := os.Stat(filepath.Join(root, name))
	return err == nil
}

func hasPython(root string) bool {
	m, _ := filepath.Glob(filepath.Join(root, "*.py"))
	return len(m) > 0
}

func hasPythonTests(root string) bool {
	found := false
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || found {
			return filepath.SkipAll
		}
		if d.IsDir() && skipDirs[d.Name()] {
			return filepath.SkipDir
		}
		n := d.Name()
		if !d.IsDir() && strings.HasSuffix(n, ".py") && (strings.HasPrefix(n, "test_") || strings.HasSuffix(n, "_test.py")) {
			found = true
		}
		return nil
	})
	return found
}

// RunHooks запускает хуки по очереди. ok=false — хотя бы один упал; report —
// вывод упавших хуков, который вернётся агенту.
func RunHooks(ctx context.Context, root string, hooks []Hook) (ok bool, report string) {
	var b strings.Builder
	ok = true
	for _, h := range hooks {
		if len(h.Cmd) == 0 {
			continue
		}
		hctx, cancel := context.WithTimeout(ctx, hookTimeout)
		cmd := exec.CommandContext(hctx, h.Cmd[0], h.Cmd[1:]...)
		cmd.Dir = root
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		cancel()
		if err != nil {
			ok = false
			fmt.Fprintf(&b, "✗ %s (%s):\n%s\n", h.Name, strings.Join(h.Cmd, " "), lastRunes(out.String(), 4000))
		} else {
			fmt.Fprintf(&b, "✓ %s\n", h.Name)
		}
	}
	return ok, b.String()
}

func lastRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return "…" + string(r[len(r)-n:])
	}
	return s
}
