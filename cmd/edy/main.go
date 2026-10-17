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
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/laurenwilcox/name/internal/omniroute"
	"github.com/laurenwilcox/name/internal/store"
	"github.com/laurenwilcox/name/internal/vault"
)

const settingRouterToken = "router_token"

const usage = `Edy — ядро ИИ-IDE.

Использование:
  edy serve [-addr 127.0.0.1:11435]        запустить OmniRoute
  edy token                                показать токен доступа к OmniRoute
  edy provider add -name N -kind K [...]   подключить провайдера (kind: openai | anthropic | cli)
  edy provider list                        список провайдеров
  edy key add -provider N [-label L]       добавить API-ключ (читается из stdin)
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
		return serve(st, home, args[1:])
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
	case cmd == "key add":
		return keyAdd(st, args[2:])
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

func serve(st *store.Store, home string, args []string) error {
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
	srv := &http.Server{Addr: *addr, Handler: omniroute.NewServer(st, tok, log), ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()

	log.Info("OmniRoute запущен", "addr", "http://"+*addr+"/v1", "token_file", filepath.Join(home, "router.token"))
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func providerAdd(st *store.Store, args []string) error {
	fs := flag.NewFlagSet("provider add", flag.ContinueOnError)
	p := store.Provider{}
	var models, preset, command string
	fs.StringVar(&p.Name, "name", "", "имя провайдера — префикс модели, например anthropic")
	fs.StringVar(&p.Kind, "kind", "", "тип: openai | anthropic | cli")
	fs.StringVar(&preset, "preset", "", "для cli: готовая команда — ollama | claude-code | aider")
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
			var ok bool
			if p.Command, ok = omniroute.CLIPresets[preset]; !ok {
				return fmt.Errorf("неизвестный -preset %q", preset)
			}
		case command != "":
			if err := json.Unmarshal([]byte(command), &p.Command); err != nil {
				return fmt.Errorf("-command должен быть JSON-массивом строк: %w", err)
			}
		default:
			return errors.New("для -kind cli нужен -preset или -command")
		}
	default:
		return errors.New("-kind должен быть openai, anthropic или cli")
	}
	p.Models = strings.Split(models, ",")
	if _, err := st.AddProvider(p); err != nil {
		return err
	}
	fmt.Printf("Провайдер %q добавлен. Модели вызываются как \"%s/<модель>\".\n", p.Name, p.Name)
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
