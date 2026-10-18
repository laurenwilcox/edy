// Package app собирает Edy целиком: OmniRoute, API, веб-интерфейс, браузеры
// для веб-чатов и фоновые задачи агентов. Им пользуются и `edy serve`/`edy ui`,
// и окно Wails — поэтому они работают одинаково.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/laurenwilcox/name/internal/agent"
	"github.com/laurenwilcox/name/internal/api"
	"github.com/laurenwilcox/name/internal/logbuf"
	"github.com/laurenwilcox/name/internal/omniroute"
	"github.com/laurenwilcox/name/internal/review"
	"github.com/laurenwilcox/name/internal/semantic"
	"github.com/laurenwilcox/name/internal/store"
	"github.com/laurenwilcox/name/internal/tasks"
	"github.com/laurenwilcox/name/internal/ui"
	"github.com/laurenwilcox/name/internal/vault"
	"github.com/laurenwilcox/name/internal/webchat"
)

const settingRouterToken = "router_token"

// Home — папка данных Edy: $EDY_HOME или ~/Library/Application Support/Edy.
func Home() (string, error) {
	home := os.Getenv("EDY_HOME")
	if home == "" {
		cfg, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(cfg, "Edy")
	}
	return home, os.MkdirAll(home, 0o700)
}

// OpenStore открывает базу Edy; мастер-ключ берётся из Keychain.
func OpenStore() (*store.Store, string, error) {
	home, err := Home()
	if err != nil {
		return nil, "", err
	}
	v, err := vault.FromKeychain()
	if err != nil {
		return nil, "", err
	}
	st, err := store.Open(filepath.Join(home, "edy.db"), v)
	return st, home, err
}

// EnsureToken возвращает токен OmniRoute, создавая его при первом запуске.
// Копия кладётся в файл router.token (0600), чтобы внешние программы могли его прочитать.
func EnsureToken(st *store.Store, home string) (string, error) {
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

// Server — запущенный Edy.
type Server struct {
	Addr  string
	Token string
	srv   *http.Server
	web   *webchat.Manager
	api   *api.Handler
	done  chan error
}

// URL — адрес интерфейса с токеном в #-части (браузер не отправляет её на сервер).
func (s *Server) URL() string { return "http://" + s.Addr + "/#token=" + s.Token }

// Done закрывается с ошибкой, если сервер остановился сам.
func (s *Server) Done() <-chan error { return s.done }

// Logs — журнал работы для экрана «Логи»; дублируется в stderr.
var Logs = logbuf.New(os.Stderr)

// Start поднимает Edy на addr и сразу начинает принимать запросы.
func Start(st *store.Store, home, addr string, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	tok, err := EnsureToken(st, home)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("не удалось занять %s (уже запущен другой Edy?): %w", addr, err)
	}

	web := webchat.NewManager(home)
	web.Log = Logs

	router := omniroute.NewServer(st, tok, log)
	router.RegisterAdapter(store.KindWeb, &omniroute.WebAdapter{Browser: web})
	h := api.New(st, tok, ln.Addr().String())
	h.Web = web
	h.Logs = Logs
	h.LLM = &agent.OmniRoute{BaseURL: "http://edy.local/v1", Token: tok, Client: "orchestrator", HTTP: agent.InProcess(router)}
	h.Tasks = tasks.NewManager(&agent.OmniRoute{BaseURL: "http://edy.local/v1", Token: tok, Client: "agent", HTTP: agent.InProcess(router)}, st)
	h.Tasks.Journal = st
	h.Tasks.SkillsHome = home
	h.Tasks.Notes = st
	h.Semantic = semantic.New(&semantic.Client{BaseURL: "http://edy.local/v1", Token: tok, Client: "embeddings", HTTP: agent.InProcess(router)}, st)
	h.Tasks.Semantic = h.Semantic
	h.Tasks.Team = st
	h.Tasks.AttachDir = filepath.Join(home, "attachments")
	h.Tasks.CanSee = func(model string) bool {
		name, _, _ := strings.Cut(model, "/")
		p, err := st.Provider(name)
		return err == nil && omniroute.HasCap(p, omniroute.CapVision)
	}
	h.Tasks.MCP = h.MCPBridge()
	h.Tasks.Review = review.New(&agent.OmniRoute{BaseURL: "http://edy.local/v1", Token: tok, Client: "reviewer", HTTP: agent.InProcess(router)}, st,
		func(v tasks.View) string {
			ps, _ := st.ProjectSettings(v.Project)
			switch ps.ReviewModel {
			case "off":
				return ""
			case "":
				return review.AutoModel(v)
			}
			return ps.ReviewModel
		})
	h.Tasks.Notify = func(v tasks.View, msg string) {
		if off, _ := st.Setting(api.SettingNotifyOff); off == "1" {
			return
		}
		if err := h.Notify("Edy · "+filepath.Base(v.Project), v.Title+"\n"+msg); err != nil {
			log.Warn("уведомление не показано", "err", err)
		}
	}
	h.Tasks.MaxParallel = func(project string) int {
		ps, _ := st.ProjectSettings(project)
		return ps.MaxParallel
	}
	h.Tasks.AutoApprove = func(project string) bool {
		ps, err := st.ProjectSettings(project)
		return err == nil && ps.AutoApprove
	}
	h.Tasks.Finalize = api.Finalizer(st)
	h.ReposDir = filepath.Join(home, "repos")
	h.Home = home
	if n, err := st.InterruptRunningAgents(); err == nil && n > 0 {
		log.Info("агенты прошлого запуска помечены как прерванные", "count", n)
	}

	// Задачи, оборванные прошлым закрытием Edy, откатываются; очереди продолжаются.
	h.Tasks.Recover()

	mux := http.NewServeMux()
	mux.Handle("/v1/", router)
	mux.Handle("/healthz", router)
	mux.Handle("/api/", h)
	mux.Handle("/mcp", h.MCP())
	mux.Handle("/", ui.Handler())

	s := &Server{Addr: ln.Addr().String(), Token: tok, web: web, api: h, done: make(chan error, 1),
		srv: &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}}
	go func() {
		err := s.srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		s.done <- err
		close(s.done)
	}()
	log.Info("OmniRoute запущен", "addr", "http://"+addr+"/v1", "token_file", filepath.Join(home, "router.token"))
	return s, nil
}

// Close останавливает сервер и закрывает управляемые браузеры.
func (s *Server) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.srv.Shutdown(ctx)
	s.api.CloseMCP()
	s.web.Close()
}

// Running проверяет, отвечает ли Edy на addr (например, уже запущен edy serve).
func Running(addr string) bool {
	c := http.Client{Timeout: time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
