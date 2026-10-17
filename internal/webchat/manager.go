package webchat

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// ErrLoginRequired — на странице нет поля ввода: скорее всего, сессия
// закончилась и нужно войти в аккаунт заново.
var ErrLoginRequired = errors.New("нужно войти в аккаунт")

// Manager держит по одному браузеру на сайт, каждый в своём профиле
// (свои cookies и кэш), и следит, чтобы браузер был жив.
type Manager struct {
	DataDir string
	// Headless и ExtraFlags нужны для тестов; в работе окно видимое.
	Headless   bool
	ExtraFlags []string
	// Log получает сообщения о скачивании и перезапусках.
	Log io.Writer

	mu       sync.Mutex
	browsers map[string]*browser
	sites    map[string]Site

	limitMu sync.Mutex
	limits  map[string]*siteLimit
}

type browser struct {
	cmd      *exec.Cmd
	done     chan struct{} // закрывается, когда процесс Chrome завершился
	cancel   context.CancelFunc
	browser  context.Context // контекст подключения к браузеру
	tab      context.Context // рабочая вкладка
	tabClose context.CancelFunc
}

type siteLimit struct {
	mu       sync.Mutex // один запрос к сайту за раз
	lastDone time.Time
	day      string
	count    int
}

func NewManager(dataDir string) *Manager {
	return &Manager{DataDir: dataDir, Log: io.Discard, browsers: map[string]*browser{}, limits: map[string]*siteLimit{}}
}

// Sites возвращает актуальный список сайтов (перечитывает web-sites.json).
func (m *Manager) Sites() (map[string]Site, error) {
	sites, err := LoadSites(m.DataDir)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.sites = sites
	m.mu.Unlock()
	return sites, nil
}

func (m *Manager) site(name string) (Site, error) {
	sites, err := m.Sites()
	if err != nil {
		return Site{}, err
	}
	s, ok := sites[name]
	if !ok {
		return Site{}, fmt.Errorf("неизвестный веб-чат %q", name)
	}
	return s, nil
}

// EnsureChrome возвращает путь к Chrome, при необходимости скачивая его.
func (m *Manager) EnsureChrome(ctx context.Context) (string, error) {
	if p, err := FindChrome(m.DataDir); err == nil {
		return p, nil
	}
	return DownloadChrome(ctx, m.DataDir, m.Log)
}

// ensure возвращает живой браузер для сайта, запуская или перезапуская его.
func (m *Manager) ensure(ctx context.Context, name string) (*browser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b := m.browsers[name]; b != nil {
		if b.alive() {
			return b, nil
		}
		fmt.Fprintf(m.Log, "Браузер для %s не отвечает — перезапускаю\n", name)
		b.stop()
		delete(m.browsers, name)
	}
	chrome, err := m.EnsureChrome(ctx)
	if err != nil {
		return nil, err
	}
	b, err := m.launch(chrome, name)
	if err != nil {
		return nil, err
	}
	m.browsers[name] = b
	return b, nil
}

func (m *Manager) profileDir(name string) string {
	return filepath.Join(m.DataDir, "web-profiles", name)
}

func (m *Manager) launch(chrome, name string) (*browser, error) {
	profile := m.profileDir(name)
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return nil, err
	}
	portFile := filepath.Join(profile, "DevToolsActivePort")
	os.Remove(portFile)

	args := []string{
		"--user-data-dir=" + profile,
		"--remote-debugging-port=0",
		"--no-first-run",
		"--no-default-browser-check",
		// Ответ должен продолжать генерироваться, даже если окно свёрнуто.
		"--disable-background-timer-throttling",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
	}
	if m.Headless {
		args = append(args, "--headless=new")
	}
	args = append(args, m.ExtraFlags...)
	args = append(args, "about:blank")

	cmd := exec.Command(chrome, args...)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("не удалось запустить Chrome: %w", err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()

	ws, err := waitDevToolsURL(portFile, done, 30*time.Second)
	if err != nil {
		cmd.Process.Kill()
		return nil, err
	}
	allocCtx, allocCancel := chromedp.NewRemoteAllocator(context.Background(), ws)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	if err := chromedp.Run(browserCtx); err != nil {
		browserCancel()
		allocCancel()
		cmd.Process.Kill()
		return nil, fmt.Errorf("не удалось подключиться к Chrome: %w", err)
	}
	return &browser{cmd: cmd, done: done, browser: browserCtx, cancel: func() { browserCancel(); allocCancel() }}, nil
}

// waitDevToolsURL ждёт, пока Chrome запишет порт отладки в профиль.
func waitDevToolsURL(portFile string, done <-chan struct{}, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return "", errors.New("Chrome завершился сразу после запуска (возможно, этот профиль уже открыт в другом окне Chrome)")
		default:
		}
		if f, err := os.Open(portFile); err == nil {
			sc := bufio.NewScanner(f)
			var lines []string
			for sc.Scan() {
				lines = append(lines, strings.TrimSpace(sc.Text()))
			}
			f.Close()
			if len(lines) >= 2 && lines[0] != "" {
				return "ws://127.0.0.1:" + lines[0] + lines[1], nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", errors.New("Chrome не открыл порт отладки за отведённое время")
}

func (b *browser) alive() bool {
	select {
	case <-b.done:
		return false
	default:
	}
	ctx, cancel := context.WithTimeout(b.browser, 5*time.Second)
	defer cancel()
	return chromedp.Run(ctx, chromedp.ActionFunc(func(context.Context) error { return nil })) == nil
}

func (b *browser) stop() {
	if b.tabClose != nil {
		b.tabClose()
	}
	b.cancel()
	if b.cmd.Process != nil {
		b.cmd.Process.Kill()
	}
	select {
	case <-b.done:
	case <-time.After(5 * time.Second):
	}
}

// workTab возвращает рабочую вкладку, создавая её заново, если пользователь её закрыл.
func (b *browser) workTab() (context.Context, error) {
	if b.tab != nil {
		ctx, cancel := context.WithTimeout(b.tab, 5*time.Second)
		err := chromedp.Run(ctx, chromedp.Evaluate("1", nil))
		cancel()
		if err == nil {
			return b.tab, nil
		}
		b.tabClose()
	}
	tab, cancel := chromedp.NewContext(b.browser)
	if err := chromedp.Run(tab); err != nil {
		cancel()
		return nil, err
	}
	b.tab, b.tabClose = tab, cancel
	return tab, nil
}

// Close закрывает все браузеры.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, b := range m.browsers {
		b.stop()
		delete(m.browsers, name)
	}
}

// OpenLogin открывает сайт в видимом окне, чтобы пользователь вошёл в аккаунт.
func (m *Manager) OpenLogin(ctx context.Context, name string) error {
	s, err := m.site(name)
	if err != nil {
		return err
	}
	b, err := m.ensure(ctx, name)
	if err != nil {
		return err
	}
	tab, err := b.workTab()
	if err != nil {
		return err
	}
	return chromedp.Run(tab, chromedp.Navigate(s.URL))
}

// acquire соблюдает вежливость: один запрос к сайту за раз, пауза между
// запросами и дневной лимит. Возвращает функцию освобождения.
func (m *Manager) acquire(ctx context.Context, name string, s Site) (func(), error) {
	m.limitMu.Lock()
	l := m.limits[name]
	if l == nil {
		l = &siteLimit{}
		m.limits[name] = l
	}
	m.limitMu.Unlock()

	locked := make(chan struct{})
	go func() { l.mu.Lock(); close(locked) }()
	select {
	case <-locked:
	case <-ctx.Done():
		go func() { <-locked; l.mu.Unlock() }()
		return nil, ctx.Err()
	}
	release := func() { l.lastDone = time.Now(); l.mu.Unlock() }

	today := time.Now().Format("2006-01-02")
	if l.day != today {
		l.day, l.count = today, 0
	}
	if s.DailyLimit > 0 && l.count >= s.DailyLimit {
		l.mu.Unlock()
		return nil, fmt.Errorf("дневной лимит запросов к %s исчерпан (%d) — так меньше риск блокировки", s.Title, s.DailyLimit)
	}
	if wait := time.Until(l.lastDone.Add(s.minInterval())); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			l.mu.Unlock()
			return nil, ctx.Err()
		}
	}
	l.count++
	return release, nil
}

// pageState — то, что опрашивается на странице во время генерации.
type pageState struct {
	Count int    `json:"count"`
	Text  string `json:"text"`
	Busy  bool   `json:"busy"`
	Input bool   `json:"input"`
}

const stateJS = `(() => {
  const all = document.querySelectorAll(%q);
  const last = all[all.length - 1];
  const busy = %q !== "" && !!document.querySelector(%q);
  return { count: all.length, text: last ? last.innerText : "", busy, input: !!document.querySelector(%q) };
})()`

// Ask отправляет prompt в веб-чат и вызывает onText с полным текстом ответа
// по мере его появления.
func (m *Manager) Ask(ctx context.Context, name, prompt string, onText func(full string) error) error {
	s, err := m.site(name)
	if err != nil {
		return err
	}
	release, err := m.acquire(ctx, name, s)
	if err != nil {
		return err
	}
	defer release()

	b, err := m.ensure(ctx, name)
	if err != nil {
		return err
	}
	tab, err := b.workTab()
	if err != nil {
		return err
	}
	// Действия во вкладке отменяются вместе с запросом клиента.
	run := func(timeout time.Duration, actions ...chromedp.Action) error {
		tctx, cancel := context.WithTimeout(tab, timeout)
		defer cancel()
		stop := context.AfterFunc(ctx, cancel)
		defer stop()
		return chromedp.Run(tctx, actions...)
	}
	state := func() (pageState, error) {
		var st pageState
		err := run(10*time.Second, chromedp.Evaluate(fmt.Sprintf(stateJS, s.Response, s.Busy, s.Busy, s.Input), &st))
		return st, err
	}

	if err := run(60*time.Second, chromedp.Navigate(s.URL)); err != nil {
		return m.fail(tab, name, fmt.Errorf("не удалось открыть %s: %w", s.URL, err))
	}
	if err := run(30*time.Second, chromedp.WaitVisible(s.Input, chromedp.ByQuery)); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return m.fail(tab, name, fmt.Errorf("%w в %s: на странице нет поля ввода. Нажмите «Войти» у провайдера в Edy", ErrLoginRequired, s.Title))
	}
	before, err := state()
	if err != nil {
		return err
	}

	// Текст вставляется целиком, как при вставке из буфера обмена.
	if err := run(30*time.Second,
		chromedp.Click(s.Input, chromedp.ByQuery),
		chromedp.ActionFunc(func(c context.Context) error { return input.InsertText(prompt).Do(c) }),
		chromedp.Sleep(400*time.Millisecond),
	); err != nil {
		return m.fail(tab, name, fmt.Errorf("не удалось ввести текст: %w", err))
	}
	send := chromedp.KeyEvent(kb.Enter)
	if s.Send != "" {
		send = chromedp.Click(s.Send, chromedp.ByQuery)
	}
	if err := run(30*time.Second, send); err != nil {
		return m.fail(tab, name, fmt.Errorf("не удалось отправить сообщение: %w", err))
	}

	// Ждём появления нового ответа, затем следим за ним, пока он не перестанет меняться.
	const poll = 400 * time.Millisecond
	started := time.Now()
	var last string
	var lastChange time.Time
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
		st, err := state()
		if err != nil {
			return err
		}
		if st.Count <= before.Count && st.Text == before.Text {
			if time.Since(started) > 90*time.Second {
				return m.fail(tab, name, fmt.Errorf("%s не начал отвечать за 90 секунд", s.Title))
			}
			continue
		}
		if st.Text != last {
			last, lastChange = st.Text, time.Now()
			if err := onText(st.Text); err != nil {
				return err
			}
			continue
		}
		if !st.Busy && st.Text != "" && time.Since(lastChange) > 1500*time.Millisecond {
			return nil
		}
		if time.Since(started) > 15*time.Minute {
			return fmt.Errorf("%s отвечает дольше 15 минут", s.Title)
		}
	}
}

// fail сохраняет снимок страницы для диагностики и дописывает путь к ошибке.
func (m *Manager) fail(tab context.Context, name string, err error) error {
	dir := filepath.Join(m.DataDir, "web-debug")
	if os.MkdirAll(dir, 0o700) != nil {
		return err
	}
	var png []byte
	ctx, cancel := context.WithTimeout(tab, 10*time.Second)
	defer cancel()
	if chromedp.Run(ctx, chromedp.FullScreenshot(&png, 70)) != nil || len(png) == 0 {
		return err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%s.jpg", name, time.Now().Format("20060102-150405")))
	if os.WriteFile(path, png, 0o600) != nil {
		return err
	}
	return fmt.Errorf("%w (снимок страницы: %s)", err, path)
}
