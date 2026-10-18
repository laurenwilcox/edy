// Package notify — системные уведомления macOS («задача готова», «нужна
// помощь»). Используется встроенный osascript: без CGO и сторонних
// программ. Текст передаётся аргументами, а не внутри скрипта, поэтому
// название задачи не может выполниться как AppleScript.
package notify

import (
	"context"
	"os/exec"
	"runtime"
	"time"
)

// Notifier показывает уведомление. Подменяется в тестах.
type Notifier func(title, message string) error

// Run запускает команду; подменяется в тестах.
var Run = func(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}

const script = `on run argv
display notification (item 2 of argv) with title (item 1 of argv) sound name "Glass"
end run`

// System показывает уведомление macOS; на других системах ничего не делает.
func System(title, message string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return Run(ctx, "osascript", "-e", script, Clip(title, 120), Clip(message, 400))
}

// Clip обрезает текст до n символов.
func Clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
