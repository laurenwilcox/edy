// Package webchat управляет браузером и ведёт диалог с веб-чатами (ChatGPT,
// Claude.ai, Gemini) от имени пользователя, в его собственных аккаунтах.
//
// Принцип: вести себя как обычный человек, а не маскироваться. Обычный Chrome
// с видимым окном, ручной вход, один запрос за раз с паузами и дневным лимитом.
// Никакой подмены отпечатков браузера и обхода капчи.
package webchat

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Виды браузера для веб-подключения.
const (
	// BrowserChromium — изолированный Chromium, который Edy скачивает в свою папку.
	BrowserChromium = "chromium"
	// BrowserChrome — установленный у пользователя Google Chrome, но с отдельной
	// папкой профиля Edy: обычные вкладки и аккаунты пользователя не затрагиваются.
	BrowserChrome = "chrome"
)

// chromiumSnapshots — официальные сборки Chromium от проекта Chromium.
const chromiumSnapshots = "https://commondatastorage.googleapis.com/chromium-browser-snapshots"

// FindBrowser ищет исполняемый файл браузера нужного вида.
// $EDY_CHROME подменяет браузер (для тестов).
func FindBrowser(dataDir, kind string) (string, error) {
	if p := os.Getenv("EDY_CHROME"); p != "" {
		return p, nil
	}
	var candidates []string
	switch kind {
	case BrowserChromium:
		base := filepath.Join(dataDir, "chromium")
		candidates = []string{
			filepath.Join(base, "chrome-mac", "Chromium.app", "Contents", "MacOS", "Chromium"),
			filepath.Join(base, "chrome-linux", "chrome"),
		}
	case BrowserChrome:
		home, _ := os.UserHomeDir()
		candidates = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			filepath.Join(home, "Applications/Google Chrome.app/Contents/MacOS/Google Chrome"),
			"/usr/bin/google-chrome",
		}
	default:
		return "", fmt.Errorf("неизвестный вид браузера %q", kind)
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c, nil
		}
	}
	if kind == BrowserChrome {
		return "", errors.New("Google Chrome не установлен — выберите изолированный Chromium")
	}
	return "", errors.New("Chromium ещё не скачан")
}

func snapshotPlatform() (dir, archive string, err error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return "Mac_Arm", "chrome-mac.zip", nil
	case "darwin/amd64":
		return "Mac", "chrome-mac.zip", nil
	case "linux/amd64":
		return "Linux_x64", "chrome-linux.zip", nil
	}
	return "", "", fmt.Errorf("скачивание Chromium не поддерживается для %s/%s", runtime.GOOS, runtime.GOARCH)
}

// DownloadChromium скачивает свежую сборку Chromium в dataDir/chromium.
func DownloadChromium(ctx context.Context, dataDir string, log io.Writer) (string, error) {
	return downloadChromium(ctx, chromiumSnapshots, dataDir, log)
}

func downloadChromium(ctx context.Context, base, dataDir string, log io.Writer) (string, error) {
	platform, archive, err := snapshotPlatform()
	if err != nil {
		return "", err
	}
	var rev bytes.Buffer
	if err := download(ctx, base+"/"+platform+"/LAST_CHANGE", &rev); err != nil {
		return "", fmt.Errorf("не удалось узнать последнюю версию Chromium: %w", err)
	}
	revision := strings.TrimSpace(rev.String())
	if revision == "" || strings.ContainsAny(revision, "/.") {
		return "", fmt.Errorf("неожиданный номер сборки Chromium %q", revision)
	}
	fmt.Fprintf(log, "Скачиваю Chromium (сборка %s, %s)…\n", revision, platform)

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dataDir, "chromium-*.zip")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := download(ctx, base+"/"+platform+"/"+revision+"/"+archive, tmp); err != nil {
		return "", err
	}

	dest := filepath.Join(dataDir, "chromium")
	if err := os.RemoveAll(dest); err != nil {
		return "", err
	}
	fmt.Fprintln(log, "Распаковываю…")
	if err := unzip(tmp.Name(), dest); err != nil {
		return "", fmt.Errorf("распаковка Chromium: %w", err)
	}
	path, err := FindBrowser(dataDir, BrowserChromium)
	if err != nil {
		return "", errors.New("в скачанном архиве не найден исполняемый файл Chromium")
	}
	fmt.Fprintln(log, "Chromium готов:", path)
	return path, nil
}

func download(ctx context.Context, url string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("скачивание Chrome: HTTP %d", resp.StatusCode)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// unzip распаковывает архив с сохранением прав и символических ссылок:
// без ссылок приложение macOS (.app) не запустится.
func unzip(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	root, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	for _, f := range r.File {
		path := filepath.Join(root, f.Name)
		if path != root && !strings.HasPrefix(path, root+string(filepath.Separator)) {
			return fmt.Errorf("недопустимый путь в архиве: %s", f.Name)
		}
		mode := f.Mode()
		switch {
		case f.FileInfo().IsDir():
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
		case mode&os.ModeSymlink != 0:
			target, err := readAll(f)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(string(target), path); err != nil {
				return err
			}
		default:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := extractFile(f, path, mode.Perm()|0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

func readAll(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func extractFile(f *zip.File, path string, perm os.FileMode) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
