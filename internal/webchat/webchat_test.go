package webchat

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeChat — страница, похожая на веб-чат: поле ввода, кнопка отправки,
// ответ печатается по буквам, пока видна кнопка «Стоп».
const fakeChat = `<!doctype html><html><body>
<div id="log"></div>
<textarea id="in"></textarea><button id="send">Send</button>
<script>
document.getElementById("send").onclick = () => {
  const q = document.getElementById("in").value;
  document.getElementById("in").value = "";
  const stop = document.createElement("button"); stop.id = "stop"; document.body.append(stop);
  const m = document.createElement("div"); m.className = "bot"; document.getElementById("log").append(m);
  const answer = "Ответ на: " + q;
  let i = 0;
  const t = setInterval(() => {
    m.innerText = answer.slice(0, ++i);
    if (i >= answer.length) { clearInterval(t); stop.remove(); }
  }, 70);
};
</script></body></html>`

func chromeForTest(t *testing.T) {
	t.Helper()
	if os.Getenv("EDY_CHROME") != "" {
		return
	}
	for _, p := range []string{"/opt/pw-browsers/chromium-1194/chrome-linux/chrome", "/usr/bin/chromium", "/usr/bin/google-chrome"} {
		if _, err := os.Stat(p); err == nil {
			t.Setenv("EDY_CHROME", p)
			return
		}
	}
	t.Skip("Chrome не найден — тесты браузера пропущены")
}

func newTestManager(t *testing.T, sites map[string]Site) *Manager {
	t.Helper()
	chromeForTest(t)
	dir := t.TempDir()
	data, _ := json.Marshal(sites)
	if err := os.WriteFile(filepath.Join(dir, SitesFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(dir)
	m.Headless = true
	m.ExtraFlags = []string{"--no-sandbox", "--disable-gpu"}
	t.Cleanup(m.Close)
	return m
}

func fakeServer(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, fakeChat)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func fakeSite(url string) Site {
	return Site{Title: "Fake", URL: url, Input: "#in", Send: "#send", Response: ".bot", Busy: "#stop", MinIntervalSec: 1, DailyLimit: 3}
}

func TestAskStreamsAnswer(t *testing.T) {
	m := newTestManager(t, map[string]Site{"fake": fakeSite(fakeServer(t))})
	var updates []string
	err := m.Ask(context.Background(), "fake", "привет", func(full string) error {
		updates = append(updates, full)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) < 2 {
		t.Fatalf("ожидался потоковый ответ, обновлений: %d", len(updates))
	}
	if got := updates[len(updates)-1]; got != "Ответ на: привет" {
		t.Fatalf("ответ = %q", got)
	}
}

func TestPolitenessAndWatchdog(t *testing.T) {
	m := newTestManager(t, map[string]Site{"fake": fakeSite(fakeServer(t))})
	ask := func() error { return m.Ask(context.Background(), "fake", "q", func(string) error { return nil }) }
	if err := ask(); err != nil {
		t.Fatal(err)
	}

	// Браузер «упал» — следующий запрос должен перезапустить его сам.
	m.mu.Lock()
	m.browsers["fake"].cmd.Process.Kill()
	m.mu.Unlock()
	time.Sleep(300 * time.Millisecond)

	start := time.Now()
	if err := ask(); err != nil {
		t.Fatalf("после падения браузера: %v", err)
	}
	if time.Since(start) < time.Second {
		t.Fatal("пауза между запросами не соблюдена")
	}
	if err := ask(); err != nil {
		t.Fatal(err)
	}
	// Лимит — 3 запроса в день.
	if err := ask(); err == nil || !strings.Contains(err.Error(), "дневной лимит") {
		t.Fatalf("ожидался дневной лимит, err = %v", err)
	}
}

func TestLoginRequiredSavesSnapshot(t *testing.T) {
	s := fakeSite(fakeServer(t))
	s.Input = "#no-such-input"
	m := newTestManager(t, map[string]Site{"fake": s})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err := m.Ask(ctx, "fake", "q", func(string) error { return nil })
	if !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "снимок страницы") {
		t.Fatalf("нет снимка страницы: %v", err)
	}
}

func TestLoadSitesOverrides(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, SitesFile), []byte(`{"chatgpt":{"input":"#new-input"},"mine":{"url":"https://x","input":"#i","response":".r"}}`), 0o600)
	sites, err := LoadSites(dir)
	if err != nil {
		t.Fatal(err)
	}
	if sites["chatgpt"].Input != "#new-input" || sites["chatgpt"].URL != DefaultSites["chatgpt"].URL {
		t.Fatalf("chatgpt = %+v", sites["chatgpt"])
	}
	if sites["mine"].Response != ".r" {
		t.Fatalf("mine = %+v", sites["mine"])
	}
	os.WriteFile(filepath.Join(dir, SitesFile), []byte(`{"bad":{"url":"https://x"}}`), 0o600)
	if _, err := LoadSites(dir); err == nil {
		t.Fatal("ожидалась ошибка для неполного сайта")
	}
}

func TestDownloadChromeUnpacksWithSymlinks(t *testing.T) {
	platform, err := cftPlatform()
	if err != nil {
		t.Skip(err)
	}
	exe := map[string]string{
		"mac-arm64": "chrome-mac-arm64/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing",
		"mac-x64":   "chrome-mac-x64/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing",
		"linux64":   "chrome-linux64/chrome",
	}[platform]

	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	f, _ := zw.CreateHeader(&zip.FileHeader{Name: exe, Method: zip.Deflate, ExternalAttrs: (0o755 | 0o100000) << 16, CreatorVersion: 3 << 8})
	f.Write([]byte("#!/bin/sh\n"))
	link := &zip.FileHeader{Name: filepath.Dir(exe) + "/Current", CreatorVersion: 3 << 8}
	link.SetMode(os.ModeSymlink | 0o777)
	l, _ := zw.CreateHeader(link)
	l.Write([]byte(filepath.Base(exe)))
	zw.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.json" {
			fmt.Fprintf(w, `{"channels":{"Stable":{"version":"150.0.1","downloads":{"chrome":[{"platform":%q,"url":"%s/chrome.zip"}]}}}}`, platform, "http://"+r.Host)
			return
		}
		w.Write(zbuf.Bytes())
	}))
	defer srv.Close()

	dir := t.TempDir()
	var log bytes.Buffer
	path, err := downloadChrome(context.Background(), srv.URL+"/index.json", dir, &log)
	if err != nil {
		t.Fatal(err, log.String())
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode()&0o100 == 0 {
		t.Fatalf("исполняемый файл не распакован: %v %v", info, err)
	}
	if runtime.GOOS != "windows" {
		target, err := os.Readlink(filepath.Join(filepath.Dir(path), "Current"))
		if err != nil || target != filepath.Base(exe) {
			t.Fatalf("символическая ссылка: %q %v", target, err)
		}
	}
}

func TestUnzipRejectsPathTraversal(t *testing.T) {
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	f, _ := zw.Create("../evil")
	f.Write([]byte("x"))
	zw.Close()
	src := filepath.Join(t.TempDir(), "a.zip")
	os.WriteFile(src, zbuf.Bytes(), 0o600)
	if err := unzip(src, filepath.Join(t.TempDir(), "out")); err == nil {
		t.Fatal("архив с ../ должен отклоняться")
	}
}
