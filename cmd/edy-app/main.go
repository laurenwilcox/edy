//go:build desktop

// Окно Edy на Wails. Показывает тот же интерфейс, что `edy ui` в браузере:
// окно открывает локальный адрес Edy, поэтому потоковые ответы и всё
// остальное работают одинаково.
//
// Сборка на Mac: scripts/build-macos-app.sh (нужны Go и Xcode Command Line Tools).
package main

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"

	"github.com/laurenwilcox/name/internal/app"
	"github.com/laurenwilcox/name/internal/omniroute"
)

func main() {
	log := slog.New(slog.NewTextHandler(app.Logs, nil))
	target, srv, startErr := start(log)

	err := wails.Run(&options.App{
		Title:            "Edy",
		Width:            1440,
		Height:           900,
		MinWidth:         900,
		MinHeight:        600,
		BackgroundColour: &options.RGBA{R: 21, G: 23, B: 28, A: 255},
		AssetServer:      &assetserver.Options{Handler: bootstrap(target, startErr)},
		// Второй запуск приложения просто показывает уже открытое окно.
		SingleInstanceLock: &options.SingleInstanceLock{UniqueId: "dev.edy.ide"},
		OnShutdown: func(ctx context.Context) {
			if srv != nil {
				srv.Close()
			}
		},
		Mac: &mac.Options{
			TitleBar: mac.TitleBarDefault(),
			About:    &mac.AboutInfo{Title: "Edy", Message: "Лёгкая агентная ИИ-IDE"},
		},
	})
	if err != nil {
		log.Error("окно Edy не запустилось", "err", err)
		os.Exit(1)
	}
}

// start поднимает Edy. Если он уже запущен (например, edy serve в терминале),
// окно подключается к нему.
func start(log *slog.Logger) (string, *app.Server, error) {
	st, home, err := app.OpenStore()
	if err != nil {
		return "", nil, err
	}
	addr := omniroute.DefaultAddr
	srv, err := app.Start(st, home, addr, log)
	if err == nil {
		return srv.URL() + "&app=1", srv, nil
	}
	if app.Running(addr) {
		tok, terr := app.EnsureToken(st, home)
		if terr != nil {
			return "", nil, terr
		}
		return "http://" + addr + "/#token=" + tok + "&app=1", nil, nil
	}
	return "", nil, err
}

// bootstrap — стартовая страница окна: сразу переходит в интерфейс Edy или
// показывает, почему он не запустился.
func bootstrap(target string, startErr error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if startErr != nil {
			fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><body style="font:15px -apple-system,sans-serif;padding:40px;background:#15171c;color:#e6e8f0">
<h2>Edy не запустился</h2><p>%s</p><p style="color:#646b7d">Закройте окно и попробуйте снова. Если ошибка повторяется — запустите <code>edy serve</code> в Терминале, чтобы увидеть подробности.</p>`,
				html.EscapeString(startErr.Error()))
			return
		}
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><body style="background:#15171c"><script>location.replace(%q)</script>`, target)
	})
}
