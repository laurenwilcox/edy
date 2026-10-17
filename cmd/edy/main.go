// Команда edy — ядро Edy. Пока UI на Wails не готов, OmniRoute настраивается
// и запускается из терминала.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/laurenwilcox/name/internal/agent"
	"github.com/laurenwilcox/name/internal/api"
	"github.com/laurenwilcox/name/internal/omniroute"
	"github.com/laurenwilcox/name/internal/store"
	"github.com/laurenwilcox/name/internal/tasks"
	"github.com/laurenwilcox/name/internal/ui"
	"github.com/laurenwilcox/name/internal/vault"
	"github.com/laurenwilcox/name/internal/webchat"
)

const settingRouterToken = "router_token"

const usage = `Edy — ядро ИИ-IDE.

Использование:
  edy ui                                   запустить OmniRoute и открыть интерфейс в браузере
  edy serve [-addr 127.0.0.1:11435]        запустить OmniRoute (интерфейс — по адресу из лога)
  edy token                                показать токен доступа к OmniRoute
  edy provider add -name N -kind K [...]   подключить провайдера (kind: openai | anthropic | cli | web);
                                           для cli с -preset утилита ставится сама, если её нет
  edy provider list                        список провайдеров
  edy provider remove -name N              удалить провайдера вместе с его ключами
  edy key add -provider N [-label L]       добавить API-ключ (читается из stdin)
  edy web chromium                         скачать изолированный Chromium в папку Edy
  edy web login <провайдер>                войти в аккаунт веб-чата и проверить подключение
  edy task -model M[,M2] [-project DIR] "задача"
                                           агент выполняет задачу в проекте (Go/Python, git):
                                           хуки, снимки, Circuit Breaker, Approve в ветку edy/…
  edy budget set <USD>                     дневной лимит расходов (0 — без лимита)
  edy spend                                расходы за сегодня

Данные хранятся в $EDY_HOME (по умолчанию ~/Library/Application Support/Edy).
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}
	st, home, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	switch cmd := strings.Join(args[:min(2, len(args))], " "); {
	case args[0] == "serve":
		return serve(st, home, args[1:], false)
	case args[0] == "ui":
		return serve(st, home, args[1:], true)
	case args[0] == "token":
		tok, err := ensureToken(st, home)
		if err == nil {
			fmt.Println(tok)
		}
		return err
	case cmd == "provider add":
		return providerAdd(st, args[2:])
	case cmd == "provider list":
		return providerList(st)
	case cmd == "provider remove":
		return providerRemove(st, home, args[2:])
	case cmd == "key add":
		return keyAdd(st, args[2:])
	case cmd == "web chromium":
		web := webchat.NewManager(home)
		web.Log = os.Stdout
		path, err := web.EnsureBrowser(context.Background(), webchat.BrowserChromium)
		if err == nil {
			fmt.Println("Chromium:", path)
		}
		return err
	case cmd == "web login" && len(args) == 3:
		return webLogin(st, home, args[2])
	case args[0] == "task":
		return runTask(st, home, args[1:])
	case cmd == "budget set" && len(args) == 3:
		v, err := strconv.ParseFloat(args[2], 64)
		if err != nil || v < 0 {
			return fmt.Errorf("бюджет должен быть неотрицательным числом")
		}
		return st.SetSetting(omniroute.SettingDailyBudget, args[2])
	case args[0] == "spend":
		now := time.Now()
		spent, err := st.SpentSince(time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()))
		if err == nil {
			fmt.Printf("Сегодня потрачено: $%.4f\n", spent)
		}
		return err
	default:
		fmt.Print(usage)
		return fmt.Errorf("неизвестная команда %q", strings.Join(args, " "))
	}
}

func openStore() (*store.Store, string, error) {
	home := os.Getenv("EDY_HOME")
	if home == "" {
		cfg, err := os.UserConfigDir()
		if err != nil {
			return nil, "", err
		}
		home = filepath.Join(cfg, "Edy")
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, "", err
	}
	v, err := vault.FromKeychain()
	if err != nil {
		return nil, "", err
	}
	st, err := store.Open(filepath.Join(home, "edy.db"), v)
	return st, home, err
}

// ensureToken возвращает токен OmniRoute, создавая его при первом запуске.
// Копия кладётся в файл router.token (0600), чтобы внешние программы могли его прочитать.
func ensureToken(st *store.Store, home string) (string, error) {
	tok, err := st.Setting(settingRouterToken)
	if errors.Is(err, store.ErrNotFound) {
		if tok, err = omniroute.NewToken(); err != nil {
			return "", err
		}
		if err := st.SetSetting(settingRouterToken, tok); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	return tok, os.WriteFile(filepath.Join(home, "router.token"), []byte(tok+"\n"), 0o600)
}

func serve(st *store.Store, home string, args []string, openBrowser bool) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", omniroute.DefaultAddr, "адрес прослушивания")
	if err := fs.Parse(args); err != nil {
		return err
	}
	tok, err := ensureToken(st, home)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	web := webchat.NewManager(home)
	web.Log = os.Stderr
	defer web.Close()

	router := omniroute.NewServer(st, tok, log)
	router.RegisterAdapter(store.KindWeb, &omniroute.WebAdapter{Browser: web})
	apiHandler := api.New(st, tok, *addr)
	apiHandler.Web = web
	apiHandler.LLM = &agent.OmniRoute{BaseURL: "http://edy.local/v1", Token: tok, Client: "orchestrator", HTTP: agent.InProcess(router)}
	apiHandler.Tasks = tasks.NewManager(&agent.OmniRoute{BaseURL: "http://edy.local/v1", Token: tok, Client: "agent", HTTP: agent.InProcess(router)})
	mux := http.NewServeMux()
	mux.Handle("/v1/", router)
	mux.Handle("/healthz", router)
	mux.Handle("/api/", apiHandler)
	mux.Handle("/", ui.Handler())
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("не удалось занять %s (уже запущен другой edy?): %w", *addr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()

	// Токен передаётся в #-части адреса: браузер не отправляет её на сервер.
	uiURL := "http://" + *addr + "/#token=" + tok
	log.Info("OmniRoute запущен", "addr", "http://"+*addr+"/v1", "token_file", filepath.Join(home, "router.token"))
	fmt.Fprintf(os.Stderr, "\nИнтерфейс: %s\n\n", uiURL)
	if openBrowser {
		if err := exec.Command("open", uiURL).Start(); err != nil {
			log.Warn("не удалось открыть браузер, откройте адрес вручную", "err", err)
		}
	}
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func providerAdd(st *store.Store, args []string) error {
	fs := flag.NewFlagSet("provider add", flag.ContinueOnError)
	p := store.Provider{}
	var models, preset, command string
	fs.StringVar(&p.Site, "site", "", "для web: веб-чат — chatgpt | claude | gemini")
	fs.StringVar(&p.Browser, "browser", webchat.BrowserChromium, "для web: chromium (изолированный, скачивается) | chrome (установленный Google Chrome, отдельный профиль)")
	var yes bool
	fs.StringVar(&p.Name, "name", "", "имя провайдера — префикс модели, например anthropic")
	fs.StringVar(&p.Kind, "kind", "", "тип: openai | anthropic | cli")
	fs.StringVar(&preset, "preset", "", "для cli: готовый профиль — "+presetNames())
	fs.BoolVar(&yes, "y", false, "для cli: устанавливать утилиту без вопроса")
	fs.StringVar(&command, "command", "", `для cli: команда JSON-массивом, например '["ollama","run","{model}","{prompt}"]'`)
	fs.StringVar(&p.BaseURL, "base-url", "", "базовый URL API (для OpenAI-совместимых, например http://localhost:11434/v1)")
	fs.StringVar(&models, "models", "", "модели через запятую (для /v1/models)")
	fs.Float64Var(&p.PriceIn, "price-in", 0, "USD за 1M входных токенов")
	fs.Float64Var(&p.PriceOut, "price-out", 0, "USD за 1M выходных токенов")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if p.Name == "" || strings.Contains(p.Name, "/") {
		return errors.New("нужно -name без символа /")
	}
	switch p.Kind {
	case store.KindOpenAI, store.KindAnthropic:
	case store.KindCLI:
		switch {
		case preset != "" && command != "":
			return errors.New("укажите либо -preset, либо -command")
		case preset != "":
			pr, ok := omniroute.CLIPresets[preset]
			if !ok {
				return fmt.Errorf("неизвестный -preset %q", preset)
			}
			bin, err := ensureInstalled(pr, yes, os.Stdin, os.Stdout, runShell)
			if err != nil {
				return err
			}
			// Сохраняем полный путь: при запуске PATH может не содержать папку установки.
			p.Command = append([]string{bin}, pr.Command[1:]...)
		case command != "":
			if err := json.Unmarshal([]byte(command), &p.Command); err != nil {
				return fmt.Errorf("-command должен быть JSON-массивом строк: %w", err)
			}
		default:
			return errors.New("для -kind cli нужен -preset или -command")
		}
	case store.KindWeb:
		if p.Site == "" {
			return errors.New("для -kind web нужен -site")
		}
		if p.Browser != webchat.BrowserChromium && p.Browser != webchat.BrowserChrome {
			return errors.New("-browser должен быть chromium или chrome")
		}
		if models == "" {
			models = "chat"
		}
	default:
		return errors.New("-kind должен быть openai, anthropic, cli или web")
	}
	p.Models = strings.Split(models, ",")
	if _, err := st.AddProvider(p); errors.Is(err, store.ErrExists) {
		return fmt.Errorf("провайдер %q уже есть. Посмотреть: edy provider list, удалить: edy provider remove -name %s", p.Name, p.Name)
	} else if err != nil {
		return err
	}
	fmt.Printf("Провайдер %q добавлен. Модели вызываются как \"%s/<модель>\".\n", p.Name, p.Name)
	return nil
}

// ensureInstalled находит утилиту профиля, а если её нет — предлагает
// установить и после установки ищет снова. Возвращает абсолютный путь.
func ensureInstalled(pr omniroute.CLIPreset, yes bool, in io.Reader, out io.Writer, run func(string) error) (string, error) {
	name := pr.Command[0]
	if bin, err := omniroute.FindExecutable(name); err == nil {
		return bin, nil
	}
	fmt.Fprintf(out, "Утилита %q не установлена. Команда установки:\n\n  %s\n\n", name, pr.Install)
	if !yes {
		fmt.Fprint(out, "Установить? [y/N] ")
		answer, _ := bufio.NewReader(in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" && a != "д" && a != "да" {
			return "", fmt.Errorf("установка отменена. Установите %q сами и повторите команду", name)
		}
	}
	if err := run(pr.Install); err != nil {
		return "", fmt.Errorf("установка не удалась: %w", err)
	}
	bin, err := omniroute.FindExecutable(name)
	if err != nil {
		return "", fmt.Errorf("установка завершилась, но %q не найдена. Откройте новое окно Терминала и повторите команду", name)
	}
	fmt.Fprintf(out, "Установлено: %s\n", bin)
	return bin, nil
}

func runShell(command string) error {
	cmd := exec.Command("/bin/bash", "-c", command)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func presetNames() string {
	names := make([]string, 0, len(omniroute.CLIPresets))
	for n := range omniroute.CLIPresets {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, " | ")
}

// webLogin открывает сайт провайдера в обычном окне его браузера, ждёт, пока
// пользователь войдёт и закроет окно, и печатает результат проверки.
// Пока идёт вход, edy serve для этого провайдера лучше не использовать.
func webLogin(st *store.Store, home, provider string) error {
	p, err := st.Provider(provider)
	if err != nil || p.Kind != store.KindWeb {
		return fmt.Errorf("веб-провайдер %q не найден", provider)
	}
	web := webchat.NewManager(home)
	web.Log = os.Stdout
	defer web.Close()
	if err := web.Login(context.Background(), webchat.Conn{Profile: p.Name, Site: p.Site, Browser: p.Browser}); err != nil {
		return err
	}
	fmt.Println("Войдите в аккаунт в открывшемся окне браузера, затем закройте это окно.")
	for {
		st := web.Status(p.Name)
		switch st.State {
		case webchat.StateLoginOpen, webchat.StateChecking, webchat.StateUnknown:
			time.Sleep(500 * time.Millisecond)
			continue
		case webchat.StateOK:
			fmt.Println("✓ Подключение работает.")
			return nil
		default:
			return fmt.Errorf("подключение не работает: %s", st.Message)
		}
	}
}

// runTask запускает агента в проекте. OmniRoute работает прямо в процессе,
// поэтому edy serve для этого не нужен.
func runTask(st *store.Store, home string, args []string) error {
	fs := flag.NewFlagSet("task", flag.ContinueOnError)
	models := fs.String("model", "", "модели через запятую, от слабой к сильной: при 3 провалах задача переходит к следующей")
	project := fs.String("project", ".", "папка проекта (git-репозиторий без незакоммиченных изменений)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	goal := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if goal == "" || *models == "" {
		return errors.New(`нужны -model и текст задачи, например: edy task -model agy/claude-sonnet-4-6 "Добавь функцию Sub"`)
	}
	root, err := filepath.Abs(*project)
	if err != nil {
		return err
	}
	tok, err := ensureToken(st, home)
	if err != nil {
		return err
	}
	web := webchat.NewManager(home)
	web.Log = os.Stderr
	defer web.Close()
	router := omniroute.NewServer(st, tok, slog.New(slog.NewTextHandler(io.Discard, nil)))
	router.RegisterAdapter(store.KindWeb, &omniroute.WebAdapter{Browser: web})

	var chain []string
	for _, m := range strings.Split(*models, ",") {
		if m = strings.TrimSpace(m); m != "" {
			chain = append(chain, m)
		}
	}
	icons := map[string]string{"model": "🤖", "tool": "🔧", "checkpoint": "📸", "hooks": "🧪", "breaker": "⚡", "info": "ℹ️ ", "error": "⚠️ "}
	task := &agent.Task{
		Root:   root,
		Goal:   goal,
		Models: chain,
		LLM:    &agent.OmniRoute{BaseURL: "http://edy.local/v1", Token: tok, Client: "agent", HTTP: agent.InProcess(router)},
		OnEvent: func(e agent.Event) {
			fmt.Printf("%s %s\n", icons[e.Kind], e.Text)
		},
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	res, err := task.Run(ctx)
	if err != nil {
		return err
	}
	if res.Status != agent.StatusDone {
		return errors.New("задача не выполнена: проект возвращён в исходное состояние")
	}
	fmt.Printf("\nИтог: %s\nИзменения:\n%s\n\n", res.Summary, res.Diff)
	fmt.Print("Применить (Approve) — один коммит в новую ветку? [y — да / n — откатить] ")
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(answer)); a == "y" || a == "д" || a == "да" || a == "yes" {
		branch := agent.BranchName(goal, time.Now())
		if err := agent.Approve(context.Background(), root, branch, "Edy: "+firstLineOf(goal)+"\n\n"+res.Summary); err != nil {
			return err
		}
		fmt.Printf("✓ Готово: ветка %s, проект переключён на неё.\n", branch)
		return nil
	}
	if err := agent.Reject(context.Background(), root, res.Baseline); err != nil {
		return err
	}
	fmt.Println("Изменения откачены.")
	return nil
}

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func providerRemove(st *store.Store, home string, args []string) error {
	fs := flag.NewFlagSet("provider remove", flag.ContinueOnError)
	name := fs.String("name", "", "имя провайдера")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if p, err := st.Provider(*name); err == nil && p.Kind == store.KindWeb {
		if err := webchat.NewManager(home).RemoveProfile(p.Name); err != nil {
			return err
		}
	}
	if err := st.DeleteProvider(*name); errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("провайдер %q не найден", *name)
	} else if err != nil {
		return err
	}
	fmt.Printf("Провайдер %q удалён.\n", *name)
	return nil
}

func providerList(st *store.Store) error {
	providers, err := st.Providers()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ИМЯ\tТИП\tКЛЮЧЕЙ\tURL / КОМАНДА\tМОДЕЛИ")
	for _, p := range providers {
		keys, err := st.Keys(p.ID)
		if err != nil {
			return err
		}
		target := p.BaseURL
		if p.Kind == store.KindCLI {
			target = strings.Join(p.Command, " ")
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", p.Name, p.Kind, len(keys), target, strings.Join(p.Models, ", "))
	}
	return w.Flush()
}

func keyAdd(st *store.Store, args []string) error {
	fs := flag.NewFlagSet("key add", flag.ContinueOnError)
	name := fs.String("provider", "", "имя провайдера")
	label := fs.String("label", "", "метка ключа, например «рабочий»")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p, err := st.Provider(*name)
	if err != nil {
		return fmt.Errorf("провайдер %q: %w", *name, err)
	}
	fmt.Fprint(os.Stderr, "Вставьте API-ключ и нажмите Enter: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return err
	}
	secret := strings.TrimSpace(line)
	if secret == "" {
		return errors.New("пустой ключ")
	}
	if _, err := st.AddKey(p.ID, *label, secret); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Ключ зашифрован и сохранён.")
	return nil
}
