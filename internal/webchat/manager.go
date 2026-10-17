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
	"syscall"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

var (
	// ErrLoginRequired — на странице нет поля ввода: нужно войти в аккаунт.
	ErrLoginRequired = errors.New("нужно войти в аккаунт")
	// ErrLoginOpen — открыто окно входа; пока оно не закрыто, профиль занят.
	ErrLoginOpen = errors.New("открыто окно входа — войдите в аккаунт и закройте окно браузера")
)

// Conn — одно веб-подключение: своё окружение браузера (профиль), сайт и вид браузера.
type Conn struct {
	Profile string // имя провайдера в Edy
	Site    string // chatgpt, claude, gemini…
	Browser string // BrowserChromium или BrowserChrome
}

// Состояния подключения для интерфейса.
const (
	StateUnknown       = "unknown"        // ещё не проверялось
	StateLoginOpen     = "login_open"     // открыто окно входа
	StateChecking      = "checking"       // идёт проверка
	StateOK            = "ok"             // сайт открывается, поле ввода на месте
	StateLoginRequired = "login_required" // нужно войти
	StateError         = "error"
)

// Status — последнее известное состояние подключения.
type Status struct {
	State   string    `json:"state"`
	Message string    `json:"message,omitempty"`
	At      time.Time `json:"at,omitzero"`
}

// Manager держит отдельное окружение браузера на каждое подключение: свою
// папку профиля, процесс, паузу и лимит. Поэтому к одному сайту можно
// подключить несколько аккаунтов, и они не пересекаются.
//
// Вход в аккаунт идёт в обычном окне браузера без подключения автоматизации;
// управление включается только после того, как пользователь закрыл это окно.
type Manager struct {
	DataDir string
	// Headless и ExtraFlags нужны для тестов; в работе окно видимое.
	Headless   bool
	ExtraFlags []string
	// Log получает сообщения о скачивании и перезапусках.
	Log io.Writer

	mu       sync.Mutex
	browsers map[string]*browser  // управляемые браузеры по профилям
	logins   map[string]*exec.Cmd // открытые окна входа по профилям
	statuses map[string]Status

	limitMu sync.Mutex
	limits  map[string]*profileLimit

	downloadMu sync.Mutex // Chromium скачивается один раз, даже при параллельных запросах
}

type browser struct {
	cmd      *exec.Cmd
	done     chan struct{} // закрывается, когда процесс браузера завершился
	cancel   context.CancelFunc
	browser  context.Context // контекст подключения к браузеру
	tab      context.Context // рабочая вкладка
	tabClose context.CancelFunc
}

type profileLimit struct {
	mu       sync.Mutex // один запрос в профиле за раз
	lastDone time.Time
	day      string
	count    int
}

func NewManager(dataDir string) *Manager {
	return &Manager{
		DataDir:  dataDir,
		Log:      io.Discard,
		browsers: map[string]*browser{},
		logins:   map[string]*exec.Cmd{},
		statuses: map[string]Status{},
		limits:   map[string]*profileLimit{},
	}
}

// Sites возвращает актуальный список сайтов (перечитывает web-sites.json).
func (m *Manager) Sites() (map[string]Site, error) { return LoadSites(m.DataDir) }

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

// BrowserInstalled сообщает, доступен ли браузер нужного вида.
func (m *Manager) BrowserInstalled(kind string) bool {
	_, err := FindBrowser(m.DataDir, kind)
	return err == nil
}

// EnsureBrowser возвращает путь к браузеру, скачивая Chromium при необходимости.
func (m *Manager) EnsureBrowser(ctx context.Context, kind string) (string, error) {
	if p, err := FindBrowser(m.DataDir, kind); err == nil || kind != BrowserChromium {
		return p, err
	}
	m.downloadMu.Lock()
	defer m.downloadMu.Unlock()
	if p, err := FindBrowser(m.DataDir, kind); err == nil {
		return p, nil
	}
	return DownloadChromium(ctx, m.DataDir, m.Log)
}

// Status возвращает последнее известное состояние подключения.
func (m *Manager) Status(profile string) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.logins[profile]; ok {
		return Status{State: StateLoginOpen, Message: "Войдите в аккаунт в открытом окне и закройте его — Edy проверит подключение"}
	}
	if st, ok := m.statuses[profile]; ok {
		return st
	}
	return Status{State: StateUnknown}
}

func (m *Manager) setStatus(profile, state, msg string) {
	m.mu.Lock()
	m.statuses[profile] = Status{State: state, Message: msg, At: time.Now()}
	m.mu.Unlock()
}

func (m *Manager) profileDir(name string) string {
	return filepath.Join(m.DataDir, "web-profiles", name)
}

// validProfile не даёт выйти за пределы папки профилей.
func validProfile(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("недопустимое имя профиля %q", name)
	}
	return nil
}

func (m *Manager) baseArgs(profile string) []string {
	args := []string{
		"--user-data-dir=" + m.profileDir(profile),
		"--no-first-run",
		"--no-default-browser-check",
	}
	if m.Headless {
		args = append(args, "--headless=new")
	}
	return append(args, m.ExtraFlags...)
}

// Login открывает сайт в обычном окне браузера этого подключения — без
// управления, как будто пользователь сам запустил браузер. Когда окно
// закрывают, Edy автоматически проверяет, работает ли вход.
func (m *Manager) Login(ctx context.Context, c Conn) error {
	if err := validProfile(c.Profile); err != nil {
		return err
	}
	s, err := m.site(c.Site)
	if err != nil {
		return err
	}
	bin, err := m.EnsureBrowser(ctx, c.Browser)
	if err != nil {
		return err
	}

	m.mu.Lock()
	if _, ok := m.logins[c.Profile]; ok {
		m.mu.Unlock()
		return nil // окно входа уже открыто
	}
	// Профиль не может быть открыт двумя процессами: закрываем управляемый браузер.
	if b := m.browsers[c.Profile]; b != nil {
		b.stop()
		delete(m.browsers, c.Profile)
	}
	if err := os.MkdirAll(m.profileDir(c.Profile), 0o700); err != nil {
		m.mu.Unlock()
		return err
	}
	cmd := exec.Command(bin, append(m.baseArgs(c.Profile), s.URL)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("не удалось открыть браузер: %w", err)
	}
	m.logins[c.Profile] = cmd
	m.mu.Unlock()

	go func() {
		cmd.Wait()
		m.mu.Lock()
		delete(m.logins, c.Profile)
		m.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		m.Check(ctx, c)
	}()
	return nil
}

// Check открывает сайт под управлением и проверяет, что поле ввода на месте,
// то есть вход выполнен. Результат сохраняется в статусе подключения.
func (m *Manager) Check(ctx context.Context, c Conn) Status {
	if err := validProfile(c.Profile); err != nil {
		return Status{State: StateError, Message: err.Error()}
	}
	m.setStatus(c.Profile, StateChecking, "Проверяю подключение…")
	l, err := m.lockProfile(ctx, c.Profile)
	if err != nil {
		m.setStatus(c.Profile, StateError, err.Error())
		return m.Status(c.Profile)
	}
	defer l.mu.Unlock()
	err = m.withTab(ctx, c, func(tab context.Context, s Site, run runFunc) error {
		if err := run(60*time.Second, chromedp.Navigate(s.URL)); err != nil {
			return m.fail(tab, c.Profile, fmt.Errorf("не удалось открыть %s: %w", s.URL, err))
		}
		if err := run(20*time.Second, chromedp.WaitVisible(s.Input, chromedp.ByQuery)); err != nil {
			return m.fail(tab, c.Profile, fmt.Errorf("%w в %s", ErrLoginRequired, s.Title))
		}
		return nil
	})
	m.record(c.Profile, err)
	return m.Status(c.Profile)
}

// record переводит результат запроса или проверки в статус подключения.
func (m *Manager) record(profile string, err error) {
	switch {
	case err == nil:
		m.setStatus(profile, StateOK, "Подключение работает")
	case errors.Is(err, ErrLoginRequired):
		m.setStatus(profile, StateLoginRequired, err.Error())
	case errors.Is(err, ErrLoginOpen), errors.Is(err, context.Canceled):
		// Не меняем статус: это не результат проверки.
	default:
		m.setStatus(profile, StateError, err.Error())
	}
}

// ensure возвращает живой управляемый браузер подключения, запуская или
// перезапуская его.
func (m *Manager) ensure(ctx context.Context, c Conn) (*browser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.logins[c.Profile]; ok {
		return nil, ErrLoginOpen
	}
	if b := m.browsers[c.Profile]; b != nil {
		if b.alive() {
			return b, nil
		}
		fmt.Fprintf(m.Log, "Браузер подключения %s не отвечает — перезапускаю\n", c.Profile)
		b.stop()
		delete(m.browsers, c.Profile)
	}
	// Скачивание Chromium может занять минуты — не держим общий замок.
	m.mu.Unlock()
	bin, err := m.EnsureBrowser(ctx, c.Browser)
	m.mu.Lock()
	if err != nil {
		return nil, err
	}
	if _, ok := m.logins[c.Profile]; ok {
		return nil, ErrLoginOpen
	}
	b, err := m.launch(bin, c.Profile)
	if err != nil {
		return nil, err
	}
	m.browsers[c.Profile] = b
	return b, nil
}

func (m *Manager) launch(bin, profile string) (*browser, error) {
	dir := m.profileDir(profile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	portFile := filepath.Join(dir, "DevToolsActivePort")
	os.Remove(portFile)

	args := append(m.baseArgs(profile),
		"--remote-debugging-port=0",
		// Ответ должен продолжать генерироваться, даже если окно свёрнуто.
		"--disable-background-timer-throttling",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
		"about:blank",
	)
	cmd := exec.Command(bin, args...)
	// Своя группа процессов: при остановке закрываем и все процессы вкладок.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("не удалось запустить браузер: %w", err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()

	ws, err := waitDevToolsURL(portFile, done, 30*time.Second)
	if err != nil {
		killGroup(cmd)
		return nil, err
	}
	allocCtx, allocCancel := chromedp.NewRemoteAllocator(context.Background(), ws)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	if err := chromedp.Run(browserCtx); err != nil {
		browserCancel()
		allocCancel()
		killGroup(cmd)
		return nil, fmt.Errorf("не удалось подключиться к браузеру: %w", err)
	}
	return &browser{cmd: cmd, done: done, browser: browserCtx, cancel: func() { browserCancel(); allocCancel() }}, nil
}

// waitDevToolsURL ждёт, пока браузер запишет порт отладки в профиль.
func waitDevToolsURL(portFile string, done <-chan struct{}, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return "", errors.New("браузер завершился сразу после запуска (возможно, этот профиль уже открыт в другом окне)")
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
	return "", errors.New("браузер не открыл порт отладки за отведённое время")
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
	killGroup(b.cmd)
	select {
	case <-b.done:
	case <-time.After(5 * time.Second):
	}
	// Дочерние процессы завершаются чуть позже главного.
	time.Sleep(300 * time.Millisecond)
}

// killGroup завершает браузер вместе со всеми его процессами.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	cmd.Process.Kill()
}

// workTab возвращает рабочую вкладку, создавая её заново, если её закрыли.
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

// RemoveProfile закрывает браузер подключения и удаляет его данные (cookies, вход).
func (m *Manager) RemoveProfile(name string) error {
	if err := validProfile(name); err != nil {
		return err
	}
	m.mu.Lock()
	if b := m.browsers[name]; b != nil {
		b.stop()
		delete(m.browsers, name)
	}
	if cmd := m.logins[name]; cmd != nil {
		killGroup(cmd)
	}
	delete(m.statuses, name)
	m.mu.Unlock()
	return os.RemoveAll(m.profileDir(name))
}

// Close закрывает все управляемые браузеры. Окна входа остаются открытыми:
// их закрывает сам пользователь.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, b := range m.browsers {
		b.stop()
		delete(m.browsers, name)
	}
}

// lockProfile занимает подключение: один запрос или проверка за раз.
func (m *Manager) lockProfile(ctx context.Context, name string) (*profileLimit, error) {
	m.limitMu.Lock()
	l := m.limits[name]
	if l == nil {
		l = &profileLimit{}
		m.limits[name] = l
	}
	m.limitMu.Unlock()

	locked := make(chan struct{})
	go func() { l.mu.Lock(); close(locked) }()
	select {
	case <-locked:
		return l, nil
	case <-ctx.Done():
		go func() { <-locked; l.mu.Unlock() }()
		return nil, ctx.Err()
	}
}

// acquire соблюдает вежливость: один запрос в подключении за раз, пауза
// между запросами и дневной лимит. Возвращает функцию освобождения.
func (m *Manager) acquire(ctx context.Context, name string, s Site) (func(), error) {
	l, err := m.lockProfile(ctx, name)
	if err != nil {
		return nil, err
	}
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
	return func() { l.lastDone = time.Now(); l.mu.Unlock() }, nil
}

type runFunc func(timeout time.Duration, actions ...chromedp.Action) error

// withTab даёт рабочую вкладку подключения; действия в ней отменяются вместе с ctx.
func (m *Manager) withTab(ctx context.Context, c Conn, fn func(tab context.Context, s Site, run runFunc) error) error {
	s, err := m.site(c.Site)
	if err != nil {
		return err
	}
	b, err := m.ensure(ctx, c)
	if err != nil {
		return err
	}
	tab, err := b.workTab()
	if err != nil {
		return err
	}
	run := func(timeout time.Duration, actions ...chromedp.Action) error {
		tctx, cancel := context.WithTimeout(tab, timeout)
		defer cancel()
		stop := context.AfterFunc(ctx, cancel)
		defer stop()
		return chromedp.Run(tctx, actions...)
	}
	return fn(tab, s, run)
}

// pageState — то, что опрашивается на странице во время генерации.
type pageState struct {
	Count int    `json:"count"`
	Text  string `json:"text"`
	Busy  bool   `json:"busy"`
}

const stateJS = `(() => {
  const all = document.querySelectorAll(%q);
  const last = all[all.length - 1];
  const busy = %q !== "" && !!document.querySelector(%q);
  return { count: all.length, text: last ? last.innerText : "", busy };
})()`

// Ask отправляет prompt в веб-чат подключения и вызывает onText с полным
// текстом ответа по мере его появления.
func (m *Manager) Ask(ctx context.Context, c Conn, prompt string, onText func(full string) error) error {
	if err := validProfile(c.Profile); err != nil {
		return err
	}
	s, err := m.site(c.Site)
	if err != nil {
		return err
	}
	release, err := m.acquire(ctx, c.Profile, s)
	if err != nil {
		return err
	}
	defer release()
	err = m.withTab(ctx, c, func(tab context.Context, s Site, run runFunc) error {
		return m.ask(ctx, tab, c.Profile, s, run, prompt, onText)
	})
	m.record(c.Profile, err)
	return err
}

func (m *Manager) ask(ctx context.Context, tab context.Context, profile string, s Site, run runFunc, prompt string, onText func(string) error) error {
	state := func() (pageState, error) {
		var st pageState
		err := run(10*time.Second, chromedp.Evaluate(fmt.Sprintf(stateJS, s.Response, s.Busy, s.Busy), &st))
		return st, err
	}

	if err := run(60*time.Second, chromedp.Navigate(s.URL)); err != nil {
		return m.fail(tab, profile, fmt.Errorf("не удалось открыть %s: %w", s.URL, err))
	}
	if err := run(30*time.Second, chromedp.WaitVisible(s.Input, chromedp.ByQuery)); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return m.fail(tab, profile, fmt.Errorf("%w в %s: на странице нет поля ввода. Нажмите «Войти» у провайдера в Edy", ErrLoginRequired, s.Title))
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
		return m.fail(tab, profile, fmt.Errorf("не удалось ввести текст: %w", err))
	}
	var send chromedp.Action = chromedp.KeyEvent(kb.Enter)
	if s.Send != "" {
		send = chromedp.Click(s.Send, chromedp.ByQuery)
	}
	if err := run(30*time.Second, send); err != nil {
		return m.fail(tab, profile, fmt.Errorf("не удалось отправить сообщение: %w", err))
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
				return m.fail(tab, profile, fmt.Errorf("%s не начал отвечать за 90 секунд", s.Title))
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
func (m *Manager) fail(tab context.Context, profile string, err error) error {
	dir := filepath.Join(m.DataDir, "web-debug")
	if os.MkdirAll(dir, 0o700) != nil {
		return err
	}
	var img []byte
	ctx, cancel := context.WithTimeout(tab, 10*time.Second)
	defer cancel()
	if chromedp.Run(ctx, chromedp.FullScreenshot(&img, 70)) != nil || len(img) == 0 {
		return err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%s.jpg", profile, time.Now().Format("20060102-150405")))
	if os.WriteFile(path, img, 0o600) != nil {
		return err
	}
	return fmt.Errorf("%w (снимок страницы: %s)", err, path)
}
