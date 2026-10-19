package omniroute

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/creack/pty"
)

// extraBinDirs — папки, куда ставят утилиты установщики и Homebrew. Их часто
// нет в PATH процесса, запущенного не из настроенного терминала.
func extraBinDirs() []string {
	dirs := []string{"/opt/homebrew/bin", "/usr/local/bin"}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append([]string{filepath.Join(home, ".local", "bin")}, dirs...)
	}
	return dirs
}

// FindExecutable ищет утилиту в PATH, а затем в типичных папках установки.
// Возвращает абсолютный путь.
func FindExecutable(name string) (string, error) {
	return findExecutable(name, extraBinDirs())
}

func findExecutable(name string, extra []string) (string, error) {
	if path, err := exec.LookPath(name); err == nil {
		return filepath.Abs(path)
	}
	if !strings.ContainsRune(name, filepath.Separator) {
		for _, dir := range extra {
			path := filepath.Join(dir, name)
			if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
				return path, nil
			}
		}
	}
	return "", fmt.Errorf("утилита %q не найдена", name)
}

// WorkdirHeader задаёт рабочую папку CLI-утилиты (обычно корень проекта).
const WorkdirHeader = "X-Edy-Workdir"

// CLIAdapter запускает терминальную утилиту в виртуальном терминале (PTY):
// утилита думает, что с ней работает человек, и ведёт себя как в обычном
// терминале. Вывод очищается от цветов, спиннеров и прогресс-баров.
// На каждый запрос запускается отдельный процесс.
type CLIAdapter struct{}

func (a *CLIAdapter) Stream(ctx context.Context, call Call, emit func(Event) error) (Usage, error) {
	if len(call.Request.Tools) > 0 {
		return Usage{}, &UpstreamError{Status: http.StatusBadRequest, Message: "CLI-провайдеры не поддерживают tools"}
	}
	msgs, imgDir, cleanupImages, err := materializeImages(call.Request.Messages)
	if err != nil {
		return Usage{}, &UpstreamError{Status: http.StatusBadRequest, Message: err.Error()}
	}
	defer cleanupImages()
	prompt, err := renderPrompt(msgs)
	if err != nil {
		return Usage{}, &UpstreamError{Status: http.StatusBadRequest, Message: err.Error()}
	}
	args, err := expandCommand(call.Provider.Command, call.Model, prompt)
	if err != nil {
		return Usage{}, &UpstreamError{Status: http.StatusInternalServerError, Message: err.Error()}
	}

	bin, err := FindExecutable(args[0])
	if err != nil {
		return Usage{}, &UpstreamError{Status: http.StatusBadGateway, Message: fmt.Sprintf("%v: установите её или добавьте провайдера заново через edy provider add", err)}
	}
	if flag, ok := addDirFlag[filepath.Base(args[0])]; ok && imgDir != "" {
		// Картинки лежат вне рабочей папки — утилите нужно разрешить их читать.
		args = append([]string{args[0], flag, imgDir}, args[1:]...)
	}
	if call.MCP != nil {
		attach, ok := mcpCapable[filepath.Base(args[0])]
		if !ok {
			return Usage{}, &UpstreamError{Status: http.StatusBadRequest, Message: fmt.Sprintf("%s не умеет подключать MCP Edy", args[0])}
		}
		extra, cleanup, err := attach(*call.MCP)
		if err != nil {
			return Usage{}, &UpstreamError{Status: http.StatusInternalServerError, Message: "конфиг MCP: " + err.Error()}
		}
		defer cleanup()
		args = append(append([]string{args[0]}, extra...), args[1:]...)
	}
	cmd := exec.CommandContext(ctx, bin, args[1:]...)
	cmd.Dir = call.Workdir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "NO_COLOR=1", "CLICOLOR=0")
	// pty делает процесс лидером новой сессии, поэтому убиваем всю группу:
	// утилиты вроде aider порождают дочерние процессы.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }

	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 500, Rows: 50}) // широкий терминал — меньше переносов строк
	if err != nil {
		return Usage{}, &UpstreamError{Status: http.StatusBadGateway, Message: fmt.Sprintf("не удалось запустить %q: %v", args[0], err)}
	}
	defer tty.Close()

	var san TermSanitizer
	var output strings.Builder
	// В режиме JSON-вывода (codex --json, claude stream-json) клиенту уходит
	// только текст ответа, а квоты подписки — в лимиты провайдера.
	var jf *cliJSONFilter
	if jsonOutputCommand(args) {
		jf = &cliJSONFilter{onQuota: call.OnQuota, now: time.Now}
	}
	emitText := func(text string) error {
		if text == "" {
			return nil
		}
		output.WriteString(text)
		return emit(Event{Delta: Delta{Content: text}})
	}
	send := func(text string) error {
		if jf != nil {
			text = jf.Write(text)
		}
		return emitText(text)
	}

	if err := emit(Event{Delta: Delta{Role: "assistant"}}); err != nil {
		return Usage{}, err
	}
	buf := make([]byte, 4096)
	var emitErr error
	for {
		n, rerr := tty.Read(buf)
		if n > 0 && emitErr == nil {
			emitErr = send(san.Write(buf[:n]))
			if emitErr != nil {
				cmd.Cancel() // клиент ушёл — процесс больше не нужен
			}
		}
		if rerr != nil {
			// После выхода процесса чтение из PTY возвращает EOF (macOS) или EIO (Linux).
			if !errors.Is(rerr, io.EOF) && !errors.Is(rerr, syscall.EIO) && !errors.Is(rerr, os.ErrClosed) {
				emitErr = errors.Join(emitErr, rerr)
			}
			break
		}
	}
	// Незавершённая строка нужна для диагностики: если утилита зависла, там
	// обычно её вопрос к пользователю.
	pending := san.Pending()
	if emitErr == nil && ctx.Err() == nil {
		emitErr = send(san.Flush())
		if jf != nil && emitErr == nil {
			pending = jf.Pending()
			emitErr = emitText(jf.Flush())
		}
	}
	waitErr := cmd.Wait()

	usage := Usage{PromptTokens: estimateTokens(prompt), CompletionTokens: estimateTokens(output.String())}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	tail := lastRunes(strings.TrimSpace(output.String()+pending), 500)
	failed := waitErr != nil
	var resetAt time.Time
	if jf != nil {
		if jf.usage.TotalTokens > 0 {
			usage = jf.usage
		}
		if jf.errMsg != "" {
			tail = lastRunes(jf.errMsg, 500)
		}
		// Некоторые CLI выходят с кодом 0, хотя ответа нет, а в JSON — ошибка.
		failed = failed || (jf.texts == 0 && (jf.errMsg != "" || jf.limited))
		if jf.limited {
			resetAt = jf.resetAt
		}
		if failed && waitErr == nil {
			waitErr = errors.New("ошибка в JSON-выводе")
		}
	}
	if emitErr != nil {
		return usage, emitErr
	}
	// Отказ в правах вместо ответа: повтор даст то же самое, поэтому 422 —
	// исполнитель задачи сразу передаёт работу резервной модели.
	if ctx.Err() == nil && jf == nil && len(output.String()) < 4000 && DetectCLIToolDenied(output.String()) || jf != nil && jf.texts == 0 && DetectCLIToolDenied(tail) {
		return usage, &UpstreamError{Status: http.StatusUnprocessableEntity, Message: fmt.Sprintf(
			"%s не может работать в фоне: она хотела сама запустить свой инструмент, а без подтверждения это запрещено. "+
				"Edy правит проект своими инструментами — выберите другого исполнителя или разрешите утилите команды в её настройках. Вывод: %s",
			filepath.Base(args[0]), tail)}
	}
	if ctx.Err() != nil {
		return usage, fmt.Errorf("%s остановлена (%w), последний вывод: %q", args[0], ctx.Err(), tail)
	}
	if failed {
		// Сообщение о лимите подписки превращаем в 429: ротация уведёт провайдера на перерыв.
		if jf != nil && jf.limited {
			var wait time.Duration
			if !resetAt.IsZero() {
				wait = max(time.Until(resetAt), 0)
			}
			return usage, &UpstreamError{Status: http.StatusTooManyRequests, RetryAfter: wait, Message: fmt.Sprintf("%s упёрлась в лимит: %s", args[0], tail)}
		}
		if hit, wait := DetectCLILimit(tail); hit {
			return usage, &UpstreamError{Status: http.StatusTooManyRequests, RetryAfter: wait, Message: fmt.Sprintf("%s упёрлась в лимит: %s", args[0], tail)}
		}
		return usage, &UpstreamError{Status: http.StatusBadGateway, Message: fmt.Sprintf("%s завершилась с ошибкой (%v): %s", args[0], waitErr, tail)}
	}
	return usage, emit(Event{FinishReason: "stop"})
}

func lastRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return "…" + string(r[len(r)-n:])
	}
	return s
}

func expandCommand(template []string, model, prompt string) ([]string, error) {
	if len(template) == 0 {
		return nil, errors.New("для CLI-провайдера не задана команда запуска")
	}
	hasPrompt := false
	args := make([]string, len(template))
	for i, a := range template {
		hasPrompt = hasPrompt || strings.Contains(a, "{prompt}")
		args[i] = strings.NewReplacer("{model}", model, "{prompt}", prompt).Replace(a)
	}
	if !hasPrompt {
		return nil, errors.New("в команде CLI-провайдера нет подстановки {prompt}")
	}
	return args, nil
}

// renderPrompt сворачивает диалог в один текст: CLI-утилиты принимают
// только одну реплику. Одиночный запрос пользователя передаётся как есть.
func renderPrompt(msgs []Message) (string, error) {
	for _, m := range msgs {
		parts, err := m.Parts()
		if err != nil {
			return "", err
		}
		for _, p := range parts {
			if p.Type != "text" {
				return "", fmt.Errorf("CLI-провайдеры принимают только текст, получено %q", p.Type)
			}
		}
	}
	if len(msgs) == 1 && msgs[0].Role == "user" {
		return msgs[0].Text()
	}
	titles := map[string]string{"system": "Инструкции", "developer": "Инструкции", "user": "Пользователь", "assistant": "Ассистент", "tool": "Результат инструмента"}
	var b strings.Builder
	for _, m := range msgs {
		title, ok := titles[m.Role]
		if !ok {
			return "", fmt.Errorf("неизвестная роль сообщения %q", m.Role)
		}
		text, _ := m.Text()
		fmt.Fprintf(&b, "### %s\n%s\n\n", title, text)
	}
	b.WriteString("### Ассистент\n")
	return b.String(), nil
}

// estimateTokens грубо оценивает число токенов: CLI-утилиты его не сообщают.
func estimateTokens(s string) int64 {
	return int64((utf8.RuneCountInString(s) + 3) / 4)
}
