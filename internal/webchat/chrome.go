// Package webchat управляет Chrome и ведёт диалог с веб-чатами (ChatGPT,
// Claude.ai, Gemini) от имени пользователя, в его собственных аккаунтах.
//
// Принцип: вести себя как обычный человек, а не маскироваться. Обычный Chrome
// с видимым окном, ручной вход, один запрос за раз с паузами и дневным лимитом.
// Никакой подмены отпечатков браузера и обхода капчи.
package webchat

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// cftIndexURL — официальный список сборок Chrome for Testing от Google.
const cftIndexURL = "https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions-with-downloads.json"

// FindChrome ищет браузер: $EDY_CHROME, установленный Google Chrome или
// Chromium, затем скачанный Edy Chrome for Testing.
func FindChrome(dataDir string) (string, error) {
	if p := os.Getenv("EDY_CHROME"); p != "" {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		filepath.Join(home, "Applications/Google Chrome.app/Contents/MacOS/Google Chrome"),
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/usr/bin/google-chrome",
		"/usr/bin/chromium",
	}
	candidates = append(candidates, downloadedChromePaths(dataDir)...)
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c, nil
		}
	}
	return "", errors.New("Chrome не найден")
}

func downloadedChromePaths(dataDir string) []string {
	base := filepath.Join(dataDir, "chrome")
	return []string{
		filepath.Join(base, "chrome-mac-arm64", "Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing"),
		filepath.Join(base, "chrome-mac-x64", "Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing"),
		filepath.Join(base, "chrome-linux64", "chrome"),
	}
}

func cftPlatform() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return "mac-arm64", nil
	case "darwin/amd64":
		return "mac-x64", nil
	case "linux/amd64":
		return "linux64", nil
	}
	return "", fmt.Errorf("скачивание Chrome не поддерживается для %s/%s", runtime.GOOS, runtime.GOARCH)
}

// DownloadChrome скачивает стабильный Chrome for Testing в dataDir/chrome.
// Google не советует использовать эту сборку для обычного сёрфинга, а вход
// в аккаунт Google в ней может блокироваться — поэтому установленный Chrome
// всегда в приоритете.
func DownloadChrome(ctx context.Context, dataDir string, log io.Writer) (string, error) {
	return downloadChrome(ctx, cftIndexURL, dataDir, log)
}

func downloadChrome(ctx context.Context, indexURL, dataDir string, log io.Writer) (string, error) {
	platform, err := cftPlatform()
	if err != nil {
		return "", err
	}
	var index struct {
		Channels map[string]struct {
			Version   string `json:"version"`
			Downloads struct {
				Chrome []struct {
					Platform string `json:"platform"`
					URL      string `json:"url"`
				} `json:"chrome"`
			} `json:"downloads"`
		} `json:"channels"`
	}
	if err := getJSON(ctx, indexURL, &index); err != nil {
		return "", fmt.Errorf("не удалось получить список сборок Chrome: %w", err)
	}
	stable := index.Channels["Stable"]
	url := ""
	for _, d := range stable.Downloads.Chrome {
		if d.Platform == platform {
			url = d.URL
		}
	}
	if url == "" {
		return "", fmt.Errorf("нет сборки Chrome для %s", platform)
	}
	fmt.Fprintf(log, "Скачиваю Chrome for Testing %s (%s)…\n", stable.Version, platform)

	tmp, err := os.CreateTemp(dataDir, "chrome-*.zip")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := download(ctx, url, tmp); err != nil {
		return "", err
	}

	dest := filepath.Join(dataDir, "chrome")
	if err := os.RemoveAll(dest); err != nil {
		return "", err
	}
	fmt.Fprintln(log, "Распаковываю…")
	if err := unzip(tmp.Name(), dest); err != nil {
		return "", fmt.Errorf("распаковка Chrome: %w", err)
	}
	for _, p := range downloadedChromePaths(dataDir) {
		if _, err := os.Stat(p); err == nil {
			fmt.Fprintln(log, "Chrome готов:", p)
			return p, nil
		}
	}
	return "", errors.New("в скачанном архиве не найден исполняемый файл Chrome")
}

func getJSON(ctx context.Context, url string, v any) error {
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
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
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
