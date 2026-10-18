package webchat

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
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
	// Visible — показывать окно управляемого браузера. По умолчанию он
	// работает скрыто (headless); окно входа видно всегда.
	Visible bool
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
	// Account — Google-аккаунт (email), под которым выполнен вход в профиле;
	// пусто — аккаунт не найден, провайдер работает без него.
	Account string `json:"account,omitempty"`
}

// Manager держит отдельное окружение браузера на каждое подключение: свою
// папку профиля, процесс, паузу и лимит. Поэтому к одному сайту можно
// подключить несколько аккаунтов, и они не пересекаются.
//
// Вход в аккаунт идёт в обычном окне браузера без подключения автоматизации;
// управление включается только после того, как пользователь закрыл это окно.
type Manager struct {
	DataDir string
	// Headless и ExtraFlags нужны для тестов: Headless прячет и окно входа.
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
	cmd     *exec.Cmd
	done    chan struct{} // закрывается, когда процесс браузера завершился
	cancel  context.CancelFunc
	browser context.Context // единственная рабочая вкладка браузера
	visible bool            // запущен с видимым окном
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
	st, ok := m.statuses[profile]
	if !ok {
		st = Status{State: StateUnknown}
	}
	if _, ok := m.logins[profile]; ok {
		st = Status{State: StateLoginOpen, Message: "Войдите в аккаунт в открытом окне и закройте его — Edy проверит подключение"}
	}
	st.Account = m.account(profile)
	return st
}

// accountFile лежит внутри папки профиля и удаляется вместе с ним.
const accountFile = "edy-account.json"

// account возвращает сохранённый аккаунт профиля или пустую строку.
func (m *Manager) account(profile string) string {
	if validProfile(profile) != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(m.profileDir(profile), accountFile))
	if err != nil {
		return ""
	}
	var v struct {
		Account string `json:"account"`
	}
	if json.Unmarshal(data, &v) != nil {
		return ""
	}
	return v.Account
}

// saveAccount запоминает аккаунт профиля; пустой — стирает запись.
func (m *Manager) saveAccount(profile, account string) error {
	path := filepath.Join(m.profileDir(profile), accountFile)
	if account == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	data, _ := json.Marshal(map[string]string{"account": account})
	return os.WriteFile(path, data, 0o600)
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
		deadline := time.Now().Add(20 * time.Second)
		for {
			var v *string
			if err := run(10*time.Second, chromedp.Evaluate(fmt.Sprintf(inputJS, s.Input), &v)); err == nil && v != nil {
				// Вход выполнен — заодно узнаём, под каким аккаунтом.
				var acc string
				if err := run(15*time.Second, chromedp.Evaluate(accountJS, &acc, awaitPromise)); err == nil {
					if err := m.saveAccount(c.Profile, acc); err != nil && m.Log != nil {
						fmt.Fprintf(m.Log, "webchat: не удалось сохранить аккаунт %s: %v\n", c.Profile, err)
					}
				}
				return nil
			}
			if time.Now().After(deadline) {
				return m.fail(tab, c.Profile, fmt.Errorf("%w в %s", ErrLoginRequired, s.Title))
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
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
		switch {
		case b.visible != c.Visible:
			fmt.Fprintf(m.Log, "Режим окна подключения %s изменился — перезапускаю браузер\n", c.Profile)
		case b.alive():
			return b, nil
		default:
			fmt.Fprintf(m.Log, "Браузер подключения %s не отвечает — перезапускаю\n", c.Profile)
		}
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
	b, err := m.launch(bin, c.Profile, c.Visible)
	if err != nil {
		return nil, err
	}
	m.browsers[c.Profile] = b
	return b, nil
}

// launchArgs — аргументы запуска рабочего браузера.
func (m *Manager) launchArgs(profile string, visible bool) []string {
	args := append(m.baseArgs(profile),
		"--remote-debugging-port=0",
		// Ответ должен продолжать генерироваться, даже если окно свёрнуто.
		"--disable-background-timer-throttling",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
	)
	if !visible && !m.Headless {
		// Скрытый режим: тот же браузер и профиль, но без окна и значка в Dock.
		args = append(args, "--headless=new", "--window-size=1280,900")
	}
	return append(args, "about:blank")
}

func (m *Manager) launch(bin, profile string, visible bool) (*browser, error) {
	dir := m.profileDir(profile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	portFile := filepath.Join(dir, "DevToolsActivePort")
	os.Remove(portFile)

	cmd := exec.Command(bin, m.launchArgs(profile, visible)...)
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
	// Работаем в той вкладке, что Chrome открыл сам. chromedp.NewContext без
	// WithTargetID создал бы ещё одну, а рабочая вкладка — третью.
	pageID, err := singlePage(ws)
	if err != nil {
		killGroup(cmd)
		return nil, err
	}
	allocCtx, allocCancel := chromedp.NewRemoteAllocator(context.Background(), ws)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx, chromedp.WithTargetID(target.ID(pageID)))
	if err := chromedp.Run(browserCtx); err != nil {
		browserCancel()
		allocCancel()
		killGroup(cmd)
		return nil, fmt.Errorf("не удалось подключиться к браузеру: %w", err)
	}
	return &browser{cmd: cmd, done: done, browser: browserCtx, cancel: func() { browserCancel(); allocCancel() }, visible: visible}, nil
}

// singlePage оставляет в браузере одну вкладку и возвращает её id. Лишние
// вкладки (например, восстановленные после прошлого запуска) закрываются.
func singlePage(ws string) (string, error) {
	u, err := url.Parse(ws)
	if err != nil {
		return "", err
	}
	base := "http://" + u.Host + "/json"
	client := http.Client{Timeout: 5 * time.Second}
	var targets []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	for i := 0; i < 50; i++ {
		resp, err := client.Get(base + "/list")
		if err == nil {
			json.NewDecoder(resp.Body).Decode(&targets)
			resp.Body.Close()
		}
		hasPage := false
		for _, t := range targets {
			hasPage = hasPage || t.Type == "page"
		}
		if hasPage {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	keep := ""
	for _, t := range targets {
		if t.Type != "page" {
			continue
		}
		if keep == "" {
			keep = t.ID
			continue
		}
		if resp, err := client.Get(base + "/close/" + t.ID); err == nil {
			resp.Body.Close()
		}
	}
	if keep == "" {
		return "", errors.New("в браузере нет ни одной вкладки")
	}
	return keep, nil
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

// workTab возвращает рабочую вкладку — единственную вкладку браузера. Если
// её закрыли, браузер считается неживым и при следующем запросе перезапустится.
func (b *browser) workTab() (context.Context, error) {
	return b.browser, nil
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
	// Images — адреса крупных картинок в ответах (сгенерированных моделью).
	Images []string `json:"images"`
}

// pageJS — общие функции для скриптов на странице. Селекторы сайтов могут
// быть списками через запятую: если сайт поменял вёрстку, сработает запасной.
const pageJS = `
const vis = (el) => !!(el && (el.offsetWidth || el.offsetHeight || el.getClientRects().length));
const leaves = (sel) => {
  if (!sel) return [];
  const all = [...document.querySelectorAll(sel)];
  return all.filter((el) => !all.some((o) => o !== el && el.contains(o)));
};
const findInput = (sel) => [...document.querySelectorAll(sel)].find(vis) || null;
const inputText = (el) => (el ? (el.value !== undefined ? el.value : el.innerText) || "" : "").trim();
`

const stateJS = `(() => {` + pageJS + `
  const all = leaves(%q);
  const last = all[all.length - 1];
  const busySel = %q;
  const busy = busySel !== "" && [...document.querySelectorAll(busySel)].some(vis);
  const imgSel = %q;
  const pool = [...document.querySelectorAll(imgSel || %q)].flatMap((el) => el.tagName === "IMG" ? [el] : [...el.querySelectorAll("img")]);
  const big = (i) => vis(i) && Math.max(i.naturalWidth, i.width) >= 128 && (i.currentSrc || i.src);
  const images = [...new Set(pool.filter(big).map((i) => i.currentSrc || i.src))];
  return { count: all.length, text: last ? last.innerText.trim() : "", busy, images };
})()`

// pickUploadJS помечает поле выбора файла для картинок атрибутом
// data-edy-upload: из нескольких полей предпочитается то, что принимает image/*.
const pickUploadJS = `(() => {
  document.querySelectorAll("[data-edy-upload]").forEach((o) => o.removeAttribute("data-edy-upload"));
  const all = [...document.querySelectorAll(%q)].filter((el) => el.tagName === "INPUT" && el.type === "file" && !el.disabled);
  const el = all.find((el) => /image/.test(el.accept)) || all.find((el) => !el.accept) || all[0];
  if (!el) return false;
  el.setAttribute("data-edy-upload", "1");
  return true;
})()`

// pasteFilesJS — запасной способ приложить картинки: событие «вставить»
// с файлами в поле ввода, как при Cmd+V скриншота.
const pasteFilesJS = `(async () => {` + pageJS + `
  const el = findInput(%q);
  if (!el) return false;
  const dt = new DataTransfer();
  for (const [name, url] of %s) {
    // atob вместо fetch(data:): строгий CSP сайта может запрещать fetch.
    const [head, body] = url.split(",");
    const bytes = Uint8Array.from(atob(body), (c) => c.charCodeAt(0));
    dt.items.add(new File([bytes], name, { type: head.slice(5, head.indexOf(";")) }));
  }
  el.focus();
  el.dispatchEvent(new ClipboardEvent("paste", { clipboardData: dt, bubbles: true, cancelable: true }));
  return true;
})()`

// uploadingJS — идёт ли ещё загрузка вложений (виден индикатор прогресса).
const uploadingJS = `(() => {` + pageJS + `
  return [...document.querySelectorAll("[role='progressbar'], progress")].some(vis);
})()`

// fetchImagesJS скачивает картинки ответа в data URL с куками сайта. Не
// получилось — пустая строка, тогда картинка снимается скриншотом.
const fetchImagesJS = `(async () => {
  const out = [];
  for (const src of %s) {
    try {
      const r = await fetch(src, { credentials: "include" });
      if (!r.ok) throw new Error(r.status);
      const b = await r.blob();
      if (!b.type.startsWith("image/")) throw new Error(b.type);
      out.push(await new Promise((ok, no) => { const f = new FileReader(); f.onload = () => ok(f.result); f.onerror = no; f.readAsDataURL(b); }));
    } catch (e) {
      out.push("");
    }
  }
  return out;
})()`

// markImageJS помечает картинку с адресом src атрибутом data-edy-img.
const markImageJS = `(() => {
  document.querySelectorAll("[data-edy-img]").forEach((o) => o.removeAttribute("data-edy-img"));
  const el = [...document.querySelectorAll("img")].find((i) => (i.currentSrc || i.src) === %q);
  if (!el) return false;
  el.setAttribute("data-edy-img", "1");
  el.scrollIntoView({ block: "center" });
  return true;
})()`

// accountJS ищет email аккаунта, под которым выполнен вход: в подписях
// аватара Google (Gemini: «Аккаунт Google: Имя (email)»), затем в сессии
// ChatGPT и в аккаунте Claude.ai. Возвращает пустую строку, если не нашёл.
const accountJS = `(async () => {
  const re = /[\w.+-]+@[\w-]+(\.[\w-]+)+/;
  for (const el of document.querySelectorAll('[aria-label*="@"], [title*="@"], a[href*="accounts.google.com/SignOutOptions"]')) {
    const m = ((el.getAttribute("aria-label") || "") + " " + (el.getAttribute("title") || "")).match(re);
    if (m) return m[0];
  }
  const tryJSON = async (url, pick) => {
    try {
      const r = await fetch(url, { credentials: "include" });
      if (!r.ok) return "";
      const v = pick(await r.json());
      return typeof v === "string" && re.test(v) ? v : "";
    } catch { return ""; }
  };
  if (location.host.includes("chatgpt.com")) return await tryJSON("/api/auth/session", (j) => j && j.user && j.user.email);
  if (location.host.includes("claude.ai")) return await tryJSON("/api/account", (j) => j && (j.email_address || (j.account && j.account.email_address)));
  return "";
})()`

// inputJS возвращает текст в поле ввода или null, если поля нет.
const inputJS = `(() => {` + pageJS + `
  const el = findInput(%q);
  return el ? inputText(el) : null;
})()`

// focusJS ставит курсор в конец поля ввода.
const focusJS = `(() => {` + pageJS + `
  const el = findInput(%q);
  if (!el) return false;
  el.focus();
  if (el.isContentEditable) {
    const r = document.createRange();
    r.selectNodeContents(el);
    r.collapse(false);
    const s = window.getSelection();
    s.removeAllRanges();
    s.addRange(r);
  }
  return true;
})()`

// insertFallbackJS — запасная вставка текста: некоторые редакторы (Quill в
// Gemini) не принимают текст через Input.insertText.
const insertFallbackJS = `(() => {` + pageJS + `
  const el = findInput(%q);
  if (!el) return false;
  el.focus();
  return document.execCommand("insertText", false, %q);
})()`

// genericSend — признаки кнопки отправки, общие для большинства чатов.
const genericSend = `button[data-testid*="send" i], button[aria-label*="send" i], button[aria-label*="отправ" i], button[type="submit"]`

// clickSendJS нажимает первую видимую и активную кнопку отправки.
const clickSendJS = `(() => {` + pageJS + `
  for (const sel of [%q, %q]) {
    if (!sel) continue;
    const btn = [...document.querySelectorAll(sel)].find((b) => vis(b) && !b.disabled && b.getAttribute("aria-disabled") !== "true");
    if (btn) { btn.click(); return sel; }
  }
  return "";
})()`

// dumpJS собирает поля, кнопки и блоки сообщений страницы — по этому файлу
// легко поправить селекторы сайта в web-sites.json.
const dumpJS = `(() => {` + pageJS + `
  const keys = ["id", "class", "role", "aria-label", "data-testid", "contenteditable", "type", "placeholder", "data-message-author-role", "data-is-streaming"];
  const desc = (el) => {
    const a = { tag: el.tagName.toLowerCase(), visible: vis(el) };
    for (const k of keys) { const v = el.getAttribute(k); if (v) a[k] = v.slice(0, 160); }
    const t = (el.innerText || "").trim();
    if (t) a.text = t.slice(0, 100);
    return a;
  };
  return {
    url: location.href,
    title: document.title,
    inputs: [...document.querySelectorAll('textarea, [contenteditable="true"], input[type="text"]')].slice(0, 20).map(desc),
    buttons: [...document.querySelectorAll('button, [role="button"]')].filter(vis).slice(0, 100).map(desc),
    messages: [...document.querySelectorAll('[data-message-author-role], [data-testid*="message" i], [data-testid*="turn" i], [class*="message" i], [class*="response" i], [class*="markdown" i]')].slice(-40).map(desc),
  };
})()`

// Ask отправляет prompt и картинки (пути к файлам) в веб-чат подключения и
// вызывает onText с полным текстом ответа по мере его появления. Картинки из
// ответа дописываются в конец текста как ![image](data:...).
func (m *Manager) Ask(ctx context.Context, c Conn, prompt string, images []string, onText func(full string) error) error {
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
		return m.ask(ctx, tab, c.Profile, s, run, prompt, images, onText)
	})
	m.record(c.Profile, err)
	return err
}

func (m *Manager) ask(ctx context.Context, tab context.Context, profile string, s Site, run runFunc, prompt string, images []string, onText func(string) error) error {
	eval := func(js string, out any) error { return run(10*time.Second, chromedp.Evaluate(js, out)) }
	state := func() (pageState, error) {
		var st pageState
		err := eval(fmt.Sprintf(stateJS, s.Response, s.Busy, s.Images, s.Response), &st)
		return st, err
	}
	inputValue := func() (*string, error) {
		var v *string
		err := eval(fmt.Sprintf(inputJS, s.Input), &v)
		return v, err
	}
	sleep := func(d time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
			return nil
		}
	}

	if err := run(60*time.Second, chromedp.Navigate(s.URL)); err != nil {
		return m.fail(tab, profile, fmt.Errorf("не удалось открыть %s: %w", s.URL, err))
	}
	// Ждём поле ввода: его нет — значит, не выполнен вход.
	deadline := time.Now().Add(30 * time.Second)
	for {
		v, err := inputValue()
		if err == nil && v != nil {
			break
		}
		if time.Now().After(deadline) {
			return m.fail(tab, profile, fmt.Errorf("%w в %s: на странице нет поля ввода. Нажмите «Войти» у провайдера в Edy", ErrLoginRequired, s.Title))
		}
		if err := sleep(500 * time.Millisecond); err != nil {
			return err
		}
	}
	before, err := state()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, src := range before.Images {
		seen[src] = true
	}
	newImages := func(st pageState) []string {
		var out []string
		for _, src := range st.Images {
			if !seen[src] {
				out = append(out, src)
			}
		}
		return out
	}

	// 1. Прикладываем картинки и ждём, пока они загрузятся.
	if len(images) > 0 {
		if err := m.attach(tab, profile, s, run, eval, sleep, images); err != nil {
			return err
		}
	}

	// 2. Вставляем текст целиком, как из буфера обмена.
	var focused bool
	if err := eval(fmt.Sprintf(focusJS, s.Input), &focused); err != nil || !focused {
		return m.fail(tab, profile, errors.New("не удалось поставить курсор в поле ввода"))
	}
	if prompt != "" {
		if err := run(30*time.Second, chromedp.ActionFunc(func(c context.Context) error { return input.InsertText(prompt).Do(c) })); err != nil {
			return m.fail(tab, profile, fmt.Errorf("не удалось ввести текст: %w", err))
		}
		sleep(300 * time.Millisecond)
		if v, _ := inputValue(); v == nil || *v == "" {
			var ok bool
			eval(fmt.Sprintf(insertFallbackJS, s.Input, prompt), &ok)
			sleep(300 * time.Millisecond)
			if v, _ := inputValue(); v == nil || *v == "" {
				return m.fail(tab, profile, errors.New("текст не вставился в поле ввода"))
			}
		}
	}

	// 3. Отправляем: сначала Enter, как человек; если текст остался в поле —
	// нажимаем кнопку отправки (по селектору сайта или по общим признакам).
	// С картинками кнопка может быть неактивна, пока они загружаются, поэтому
	// нажатие повторяется до 20 секунд.
	sent := func() bool {
		if v, err := inputValue(); prompt != "" && err == nil && v != nil && *v == "" {
			return true
		}
		st, err := state()
		return err == nil && (st.Count > before.Count || st.Busy || len(newImages(st)) > 0)
	}
	if prompt != "" {
		if err := run(10*time.Second, chromedp.KeyEvent(kb.Enter)); err != nil {
			return m.fail(tab, profile, fmt.Errorf("не удалось нажать Enter: %w", err))
		}
		if err := sleep(1200 * time.Millisecond); err != nil {
			return err
		}
	}
	if !sent() {
		var clicked string
		retryUntil := time.Now()
		if len(images) > 0 {
			retryUntil = retryUntil.Add(20 * time.Second)
		}
		for {
			eval(fmt.Sprintf(clickSendJS, s.Send, genericSend), &clicked)
			if err := sleep(1500 * time.Millisecond); err != nil {
				return err
			}
			if sent() || time.Now().After(retryUntil) {
				break
			}
		}
		if !sent() {
			msg := "сообщение подготовлено, но отправить его не получилось: Enter не сработал"
			if clicked == "" {
				msg += ", кнопка отправки не найдена"
			} else {
				msg += ", нажатие кнопки тоже"
			}
			return m.fail(tab, profile, errors.New(msg))
		}
	}

	// 4. Ждём новый ответ и отдаём его по мере появления, пока он не перестанет меняться.
	const poll = 400 * time.Millisecond
	started := time.Now()
	var last, lastImages string
	var lastChange time.Time
	for {
		if err := sleep(poll); err != nil {
			return err
		}
		st, err := state()
		if err != nil {
			return err
		}
		fresh := newImages(st)
		if st.Count <= before.Count && st.Text == before.Text && len(fresh) == 0 {
			if time.Since(started) > 90*time.Second {
				return m.fail(tab, profile, fmt.Errorf("сообщение отправлено, но ответ %s не найден на странице за 90 секунд — селектор ответа (response) устарел", s.Title))
			}
			continue
		}
		if imgs := strings.Join(fresh, "\n"); imgs != lastImages {
			lastImages, lastChange = imgs, time.Now()
		}
		if st.Text != last {
			last, lastChange = st.Text, time.Now()
			if err := onText(st.Text); err != nil {
				return err
			}
			continue
		}
		if !st.Busy && (st.Text != "" || len(fresh) > 0) && time.Since(lastChange) > 1500*time.Millisecond {
			if len(fresh) == 0 {
				return nil
			}
			full := last
			for _, data := range m.grabImages(tab, run, fresh) {
				full += "\n\n![image](" + data + ")"
			}
			if full == last {
				return nil
			}
			return onText(strings.TrimLeft(full, "\n"))
		}
		if time.Since(started) > 15*time.Minute {
			return fmt.Errorf("%s отвечает дольше 15 минут", s.Title)
		}
	}
}

// attach прикладывает картинки к сообщению: через поле выбора файла, а если
// его нет — событием «вставить» в поле ввода. Затем ждёт конца загрузки.
func (m *Manager) attach(tab context.Context, profile string, s Site, run runFunc, eval func(string, any) error, sleep func(time.Duration) error, images []string) error {
	upload := s.Upload
	if upload == "" {
		upload = "input[type='file']"
	}
	var picked bool
	eval(fmt.Sprintf(pickUploadJS, upload), &picked)
	if picked {
		if err := run(30*time.Second, chromedp.SetUploadFiles("[data-edy-upload]", images, chromedp.ByQuery)); err != nil {
			return m.fail(tab, profile, fmt.Errorf("не удалось приложить картинку: %w", err))
		}
	} else {
		var files [][2]string
		for _, path := range images {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			mime := http.DetectContentType(data)
			files = append(files, [2]string{filepath.Base(path), "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)})
		}
		list, _ := json.Marshal(files)
		var ok bool
		err := run(30*time.Second, chromedp.Evaluate(fmt.Sprintf(pasteFilesJS, s.Input, list), &ok, awaitPromise))
		if err != nil || !ok {
			return m.fail(tab, profile, fmt.Errorf("на странице %s не найдено, куда приложить картинку — задайте селектор upload", s.Title))
		}
	}
	if err := sleep(2 * time.Second); err != nil {
		return err
	}
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); {
		var busy bool
		if eval(uploadingJS, &busy) != nil || !busy {
			break
		}
		if err := sleep(500 * time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

// grabImages возвращает картинки ответа как data URL: скачивает их с куками
// сайта, а если не вышло (CORS) — делает снимок элемента на странице.
func (m *Manager) grabImages(tab context.Context, run runFunc, srcs []string) []string {
	list, _ := json.Marshal(srcs)
	var data []string
	run(60*time.Second, chromedp.Evaluate(fmt.Sprintf(fetchImagesJS, list), &data, awaitPromise))
	out := make([]string, 0, len(srcs))
	for i, src := range srcs {
		if i < len(data) && strings.HasPrefix(data[i], "data:image/") {
			out = append(out, data[i])
			continue
		}
		var ok bool
		var buf []byte
		if run(10*time.Second, chromedp.Evaluate(fmt.Sprintf(markImageJS, src), &ok)) != nil || !ok {
			continue
		}
		if run(20*time.Second, chromedp.Screenshot("[data-edy-img]", &buf, chromedp.ByQuery)) == nil && len(buf) > 0 {
			out = append(out, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(buf))
		}
	}
	return out
}

// awaitPromise заставляет Evaluate дождаться результата async-функции.
func awaitPromise(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }

// fail сохраняет снимок страницы и список её полей и кнопок для диагностики
// и дописывает пути к ошибке.
func (m *Manager) fail(tab context.Context, profile string, err error) error {
	dir := filepath.Join(m.DataDir, "web-debug")
	if os.MkdirAll(dir, 0o700) != nil {
		return err
	}
	base := filepath.Join(dir, fmt.Sprintf("%s-%s", profile, time.Now().Format("20060102-150405")))
	ctx, cancel := context.WithTimeout(tab, 10*time.Second)
	defer cancel()
	var saved []string
	var img []byte
	if chromedp.Run(ctx, chromedp.FullScreenshot(&img, 70)) == nil && len(img) > 0 && os.WriteFile(base+".jpg", img, 0o600) == nil {
		saved = append(saved, base+".jpg")
	}
	var dump map[string]any
	if chromedp.Run(ctx, chromedp.Evaluate(dumpJS, &dump)) == nil {
		if data, jerr := json.MarshalIndent(dump, "", "  "); jerr == nil && os.WriteFile(base+".json", data, 0o600) == nil {
			saved = append(saved, base+".json")
		}
	}
	if len(saved) == 0 {
		return err
	}
	return fmt.Errorf("%w (диагностика: %s)", err, strings.Join(saved, ", "))
}
