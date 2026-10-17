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
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// fakeChat — страница, похожая на веб-чат: поле ввода, кнопка отправки,
// ответ печатается по буквам, пока видна кнопка «Стоп».
const fakeChat = `<!doctype html><html><body>
<div id="log"></div>
<textarea id="in"></textarea><button id="send">Send</button>
<script>
document.getElementById("send").onclick = () => {
  const q = document.getElementById("in").value.trim(); // Enter в textarea добавляет перевод строки
  document.getElementById("in").value = "";
  const stop = document.createElement("button"); stop.id = "stop"; document.body.append(stop);
  const m = document.createElement("div"); m.className = "bot"; document.getElementById("log").append(m);
  const answer = "Ответ на: " + q + ", печатаю по буквам";
  let i = 0;
  const t = setInterval(() => {
    m.innerText = answer.slice(0, ++i);
    if (i >= answer.length) { clearInterval(t); stop.remove(); }
  }, 100);
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
	t.Cleanup(func() {
		m.Close()
		m.mu.Lock()
		for _, cmd := range m.logins {
			killGroup(cmd)
		}
		m.mu.Unlock()
		time.Sleep(300 * time.Millisecond)
	})
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
	err := m.Ask(context.Background(), Conn{Profile: "fake-1", Site: "fake", Browser: BrowserChromium}, "привет", func(full string) error {
		updates = append(updates, full)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) < 2 {
		t.Fatalf("ожидался потоковый ответ, обновлений: %d", len(updates))
	}
	if got := updates[len(updates)-1]; got != "Ответ на: привет, печатаю по буквам" {
		t.Fatalf("ответ = %q", got)
	}
}

func TestPolitenessAndWatchdog(t *testing.T) {
	m := newTestManager(t, map[string]Site{"fake": fakeSite(fakeServer(t))})
	ask := func() error {
		return m.Ask(context.Background(), Conn{Profile: "acc", Site: "fake", Browser: BrowserChromium}, "q", func(string) error { return nil })
	}
	if err := ask(); err != nil {
		t.Fatal(err)
	}

	// Браузер «упал» — следующий запрос должен перезапустить его сам.
	m.mu.Lock()
	killGroup(m.browsers["acc"].cmd)
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
	err := m.Ask(ctx, Conn{Profile: "acc", Site: "fake", Browser: BrowserChromium}, "q", func(string) error { return nil })
	if !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "диагностика") {
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

func TestDownloadChromiumUnpacksWithSymlinks(t *testing.T) {
	platform, archive, err := snapshotPlatform()
	if err != nil {
		t.Skip(err)
	}
	exe := "chrome-linux/chrome"
	if strings.HasPrefix(platform, "Mac") {
		exe = "chrome-mac/Chromium.app/Contents/MacOS/Chromium"
	}
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	f, _ := zw.CreateHeader(&zip.FileHeader{Name: exe, Method: zip.Deflate, ExternalAttrs: (0o755 | 0o100000) << 16, CreatorVersion: 3 << 8})
	f.Write([]byte("#!/bin/sh\n"))
	link := &zip.FileHeader{Name: filepath.Dir(exe) + "/Current", CreatorVersion: 3 << 8}
	link.SetMode(os.ModeSymlink | 0o777)
	l, _ := zw.CreateHeader(link)
	l.Write([]byte(filepath.Base(exe)))
	zw.Close()

	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/LAST_CHANGE") {
			fmt.Fprint(w, "1500123\n")
			return
		}
		w.Write(zbuf.Bytes())
	}))
	defer srv.Close()

	t.Setenv("EDY_CHROME", "")
	dir := t.TempDir()
	var log bytes.Buffer
	path, err := downloadChromium(context.Background(), srv.URL, dir, &log)
	if err != nil {
		t.Fatal(err, log.String())
	}
	if want := "/" + platform + "/1500123/" + archive; paths[len(paths)-1] != want {
		t.Fatalf("скачан %q, ожидался %q", paths[len(paths)-1], want)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode()&0o100 == 0 {
		t.Fatalf("исполняемый файл не распакован: %v %v", info, err)
	}
	if target, err := os.Readlink(filepath.Join(filepath.Dir(path), "Current")); err != nil || target != filepath.Base(exe) {
		t.Fatalf("символическая ссылка: %q %v", target, err)
	}
}

func TestLoginWindowThenAutoCheck(t *testing.T) {
	m := newTestManager(t, map[string]Site{"fake": fakeSite(fakeServer(t))})
	c := Conn{Profile: "acc", Site: "fake", Browser: BrowserChromium}
	if st := m.Status("acc"); st.State != StateUnknown {
		t.Fatalf("начальный статус = %+v", st)
	}
	if err := m.Login(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if st := m.Status("acc"); st.State != StateLoginOpen {
		t.Fatalf("статус при открытом окне входа = %+v", st)
	}
	// Пока окно входа открыто, управлять профилем нельзя.
	if err := m.Ask(context.Background(), c, "q", func(string) error { return nil }); !errors.Is(err, ErrLoginOpen) {
		t.Fatalf("err = %v", err)
	}
	// Пользователь закрыл окно — Edy сам проверяет подключение.
	m.mu.Lock()
	killGroup(m.logins["acc"])
	m.mu.Unlock()
	deadline := time.Now().Add(60 * time.Second)
	for {
		st := m.Status("acc")
		if st.State == StateOK {
			break
		}
		if time.Now().After(deadline) || st.State == StateError || st.State == StateLoginRequired {
			t.Fatalf("после закрытия окна входа статус = %+v", st)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestCheckReportsLoginRequired(t *testing.T) {
	s := fakeSite(fakeServer(t))
	s.Input = "#no-such-input"
	m := newTestManager(t, map[string]Site{"fake": s})
	st := m.Check(context.Background(), Conn{Profile: "acc", Site: "fake", Browser: BrowserChromium})
	if st.State != StateLoginRequired {
		t.Fatalf("статус = %+v", st)
	}
}

func TestChromeKindNeedsInstalledChrome(t *testing.T) {
	t.Setenv("EDY_CHROME", "")
	if _, err := FindBrowser(t.TempDir(), BrowserChromium); err == nil {
		t.Fatal("без скачанного Chromium должна быть ошибка")
	}
	if _, err := FindBrowser(t.TempDir(), "netscape"); err == nil {
		t.Fatal("неизвестный вид браузера должен отклоняться")
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

func TestProfilesAreIsolated(t *testing.T) {
	m := newTestManager(t, map[string]Site{"fake": fakeSite(fakeServer(t))})
	for _, profile := range []string{"work", "home"} {
		if err := m.Ask(context.Background(), Conn{Profile: profile, Site: "fake", Browser: BrowserChromium}, "q", func(string) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	m.mu.Lock()
	work, home := m.browsers["work"], m.browsers["home"]
	m.mu.Unlock()
	if work == nil || home == nil || work.cmd.Process.Pid == home.cmd.Process.Pid {
		t.Fatal("у каждого профиля должен быть свой процесс браузера")
	}
	for _, p := range []string{"work", "home"} {
		if _, err := os.Stat(filepath.Join(m.DataDir, "web-profiles", p)); err != nil {
			t.Fatalf("нет папки профиля %s: %v", p, err)
		}
	}
	if err := m.RemoveProfile("work"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.DataDir, "web-profiles", "work")); !os.IsNotExist(err) {
		t.Fatal("папка удалённого профиля осталась")
	}
	if err := m.RemoveProfile("../evil"); err == nil {
		t.Fatal("имя с ../ должно отклоняться")
	}
}

// Разновидности чатов: как отправляется сообщение и где появляется ответ.
var chatVariants = map[string]string{
	// Отправка по Enter в contenteditable, кнопки нет.
	"/enter": `<!doctype html><html><body><div id="log"></div>
<div id="ed" contenteditable="true" style="min-height:20px"></div>
<script>
const ed = document.getElementById("ed");
ed.addEventListener("keydown", (e) => {
  if (e.key !== "Enter") return;
  e.preventDefault();
  const q = ed.innerText.trim(); ed.innerHTML = "";
  const m = document.createElement("div"); m.className = "answer"; m.innerHTML = '<div class="markdown"></div>';
  document.getElementById("log").append(m);
  m.firstChild.innerText = "Enter-ответ: " + q;
});
</script></body></html>`,
	// Enter ничего не делает, отправить можно только кнопкой с aria-label.
	"/button": `<!doctype html><html><body><div id="log"></div>
<textarea id="in"></textarea><button aria-label="Send message" class="x1">➤</button>
<script>
document.getElementById("in").addEventListener("keydown", (e) => { if (e.key === "Enter") e.preventDefault(); });
document.querySelector("button").onclick = () => {
  const q = document.getElementById("in").value.trim(); // Enter в textarea добавляет перевод строки document.getElementById("in").value = "";
  const m = document.createElement("div"); m.className = "answer"; m.innerText = "Кнопка-ответ: " + q;
  document.getElementById("log").append(m);
};
</script></body></html>`,
	// Ничего не отправляется.
	"/stuck": `<!doctype html><html><body><textarea id="in"></textarea><button disabled>Send</button>
<script>document.getElementById("in").addEventListener("keydown", (e) => { if (e.key === "Enter") e.preventDefault(); });</script></body></html>`,
}

func variantServer(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, chatVariants[r.URL.Path])
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestSendStrategiesAndSingleTab(t *testing.T) {
	base := variantServer(t)
	sites := map[string]Site{
		// Неверный селектор кнопки и ответа в начале списка — должны сработать запасные.
		"enter":  {Title: "Enter", URL: base + "/enter", Input: "#missing, #ed", Send: "#nope", Response: ".nope, .answer .markdown, .answer", DailyLimit: 10},
		"button": {Title: "Button", URL: base + "/button", Input: "#in", Send: "#wrong", Response: ".answer", DailyLimit: 10},
	}
	m := newTestManager(t, sites)
	for site, want := range map[string]string{"enter": "Enter-ответ: привет", "button": "Кнопка-ответ: привет"} {
		var got string
		err := m.Ask(context.Background(), Conn{Profile: site, Site: site, Browser: BrowserChromium}, "привет", func(full string) error { got = full; return nil })
		if err != nil || got != want {
			t.Fatalf("%s: got %q, err %v", site, got, err)
		}
	}

	// В браузере профиля ровно одна вкладка.
	m.mu.Lock()
	b := m.browsers["enter"]
	m.mu.Unlock()
	var pages int
	chromedp.Run(b.browser, chromedp.ActionFunc(func(ctx context.Context) error {
		infos, err := chromedp.Targets(ctx)
		for _, i := range infos {
			if i.Type == "page" {
				pages++
			}
		}
		return err
	}))
	if pages != 1 {
		t.Fatalf("вкладок: %d, ожидалась одна", pages)
	}
}

func TestStuckSendSavesDiagnostics(t *testing.T) {
	base := variantServer(t)
	m := newTestManager(t, map[string]Site{"stuck": {Title: "Stuck", URL: base + "/stuck", Input: "#in", Response: ".answer", DailyLimit: 10}})
	err := m.Ask(context.Background(), Conn{Profile: "stuck", Site: "stuck", Browser: BrowserChromium}, "привет", func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "отправить его не получилось") {
		t.Fatalf("err = %v", err)
	}
	files, _ := filepath.Glob(filepath.Join(m.DataDir, "web-debug", "stuck-*.json"))
	if len(files) != 1 {
		t.Fatalf("нет файла диагностики: %v", files)
	}
	data, _ := os.ReadFile(files[0])
	if !strings.Contains(string(data), `"inputs"`) || !strings.Contains(string(data), `"tag": "textarea"`) {
		t.Fatalf("диагностика:\n%s", data)
	}
}
