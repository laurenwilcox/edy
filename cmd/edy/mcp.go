package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/laurenwilcox/name/internal/app"
	"github.com/laurenwilcox/name/internal/mcp"
)

// runMCP — MCP-сервер Edy на stdin/stdout для Claude Code, Gemini CLI,
// Cursor и других клиентов. Если Edy уже запущен, запросы пересылаются в
// него: тогда вызовы видны в окне «Агенты», а правки блокируются, пока над
// проектом работает задача. Иначе сервер работает сам по себе.
func runMCP(home string, args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	project := fs.String("project", ".", "папка проекта")
	addr := fs.String("addr", "127.0.0.1:11435", "адрес запущенного Edy")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := filepath.Abs(*project)
	if err != nil {
		return err
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return fmt.Errorf("папка проекта %s не найдена", root)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if app.Running(*addr) {
		tok, err := os.ReadFile(filepath.Join(home, "router.token"))
		if err == nil {
			b := &bridge{base: "http://" + *addr, token: strings.TrimSpace(string(tok)), http: &http.Client{Timeout: 15 * time.Minute}}
			if err := b.register(ctx, root); err == nil {
				fmt.Fprintln(os.Stderr, "edy mcp: работаю через запущенный Edy", *addr)
				return b.serve(ctx, in, out)
			} else {
				fmt.Fprintln(os.Stderr, "edy mcp: запущенный Edy недоступен, работаю сам:", err)
			}
		}
	}
	srv := &mcp.Server{Root: root, Home: home}
	return srv.ServeStdio(ctx, in, out)
}

// bridge пересылает сообщения stdio в /mcp запущенного Edy.
type bridge struct {
	base, token string
	http        *http.Client
	project     string
	session     string
}

// register добавляет папку в проекты Edy (или поднимает её в списке).
func (b *bridge) register(ctx context.Context, root string) error {
	body, _ := json.Marshal(map[string]string{"path": root})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, b.base+"/api/projects", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var p struct {
		Path  string `json:"path"`
		Error string `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&p)
	if resp.StatusCode != http.StatusOK || p.Path == "" {
		return fmt.Errorf("HTTP %d %s", resp.StatusCode, p.Error)
	}
	b.project = p.Path
	return nil
}

func (b *bridge) serve(ctx context.Context, in io.Reader, out io.Writer) error {
	r := bufio.NewReaderSize(in, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if msg := bytes.TrimSpace(line); len(msg) > 0 {
			if resp := b.forward(ctx, msg); resp != nil {
				if _, werr := out.Write(append(resp, '\n')); werr != nil {
					return werr
				}
			}
		}
		if errors.Is(err, io.EOF) {
			b.close()
			return nil
		} else if err != nil {
			return err
		}
	}
}

func (b *bridge) forward(ctx context.Context, msg []byte) []byte {
	var head struct {
		ID json.RawMessage `json:"id"`
	}
	json.Unmarshal(msg, &head)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, b.base+"/mcp?project="+url.QueryEscape(b.project), bytes.NewReader(msg))
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if b.session != "" {
		req.Header.Set("Mcp-Session-Id", b.session)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return rpcFailure(head.ID, "Edy недоступен: "+err.Error())
	}
	defer resp.Body.Close()
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		b.session = s
	}
	body, _ := io.ReadAll(resp.Body)
	switch {
	case resp.StatusCode == http.StatusAccepted:
		return nil
	case resp.StatusCode == http.StatusOK:
		return bytes.TrimSpace(body)
	}
	var e struct {
		Error string `json:"error"`
	}
	json.Unmarshal(body, &e)
	return rpcFailure(head.ID, fmt.Sprintf("Edy: HTTP %d %s", resp.StatusCode, e.Error))
}

func (b *bridge) close() {
	if b.session == "" {
		return
	}
	req, _ := http.NewRequest(http.MethodDelete, b.base+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Mcp-Session-Id", b.session)
	if resp, err := b.http.Do(req); err == nil {
		resp.Body.Close()
	}
}

// rpcFailure — ответ JSON-RPC с ошибкой; для уведомлений (без id) — ничего.
func rpcFailure(id json.RawMessage, msg string) []byte {
	if len(id) == 0 {
		return nil
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32603, "message": msg}})
	return b
}
