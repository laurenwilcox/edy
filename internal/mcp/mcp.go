// Package mcp — MCP-сервер Edy (Model Context Protocol). Через него внешние
// агенты — Claude Code, Gemini CLI, Cursor и другие — работают с проектом
// инструментами Edy: скелет и поиск по коду, чтение и правка файлов в
// пределах проекта, проверки (хуки) и скиллы. Каждая правка сохраняется
// снимком в Shadow Git, поэтому её можно откатить из интерфейса Edy.
//
// Транспорты: stdio (`edy mcp -project …`) и Streamable HTTP (/mcp).
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/laurenwilcox/name/internal/agent"
	"github.com/laurenwilcox/name/internal/skills"
)

// Версии протокола, которые понимает сервер; первая — предпочтительная.
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// Server обслуживает один проект.
type Server struct {
	Root string
	// Home — папка данных Edy (общие скиллы в Home/skills).
	Home string
	// Version — версия Edy для serverInfo.
	Version string
	// Skills — какие скиллы видны клиенту; nil — все скиллы проекта и общие.
	Skills []string
	// Memory — память проекта (project_memory, remember); nil — без неё.
	Memory agent.Memory
	// Busy сообщает, что над проектом сейчас работает задача Edy: тогда
	// правки через MCP запрещены, чтобы не мешать агенту (блокировка файлов).
	Busy func() bool
	// OnChange вызывается после каждой правки с текстом снимка,
	// OnCall — после каждого вызова инструмента (для журнала агентов).
	OnChange func(step string)
	OnCall   func(tool, note string, isErr bool)

	mu     sync.Mutex // правки и снимки по очереди
	shadow *agent.Shadow
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Коды ошибок JSON-RPC.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// Handle обрабатывает одно сообщение JSON-RPC. Для уведомлений (без id)
// возвращает nil — отвечать на них не нужно.
func (s *Server) Handle(ctx context.Context, raw []byte) []byte {
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return encode(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{codeParse, "некорректный JSON"}})
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		if len(req.ID) == 0 {
			return nil // ответ клиента на наш запрос — мы их не шлём
		}
		return encode(response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{codeInvalidRequest, "ожидается JSON-RPC 2.0"}})
	}
	if len(req.ID) == 0 {
		return nil // notifications/initialized, notifications/cancelled и т.п.
	}
	result, rerr := s.dispatch(ctx, req)
	resp := response{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rerr}
	if rerr == nil && result == nil {
		resp.Result = struct{}{}
	}
	return encode(resp)
}

func encode(r response) []byte {
	b, _ := json.Marshal(r)
	return b
}

func (s *Server) dispatch(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		version := protocolVersions[0]
		for _, v := range protocolVersions {
			if v == p.ProtocolVersion {
				version = v
			}
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}, "prompts": map[string]any{}},
			"serverInfo":      map[string]any{"name": "edy", "title": "Edy", "version": s.version()},
			"instructions":    instructions,
		}, nil
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		return map[string]any{"tools": s.tools()}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			return nil, &rpcError{codeInvalidParams, "нужно имя инструмента"}
		}
		text, isErr := s.call(ctx, p.Name, p.Arguments)
		if s.OnCall != nil {
			s.OnCall(p.Name, note(p.Arguments, text), isErr)
		}
		return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr}, nil
	case "prompts/list":
		list, _ := s.skills()
		prompts := make([]map[string]any, 0, len(list))
		for _, sk := range list {
			prompts = append(prompts, map[string]any{"name": sk.Name, "description": sk.Description})
		}
		return map[string]any{"prompts": prompts}, nil
	case "prompts/get":
		var p struct {
			Name string `json:"name"`
		}
		json.Unmarshal(req.Params, &p)
		list, _ := s.skills()
		sk, ok := skills.Find(list, p.Name)
		if !ok {
			return nil, &rpcError{codeInvalidParams, fmt.Sprintf("скилл %q не найден", p.Name)}
		}
		return map[string]any{
			"description": sk.Description,
			"messages":    []map[string]any{{"role": "user", "content": map[string]any{"type": "text", "text": sk.Body}}},
		}, nil
	case "resources/list":
		return map[string]any{"resources": []any{}}, nil
	}
	return nil, &rpcError{codeMethodNotFound, "неизвестный метод " + req.Method}
}

const instructions = `Edy — инструменты для работы с проектом пользователя.
Начинайте с project_skeleton, ищите код через search_code, правьте через replace_in_file и write_file.
Каждая правка сохраняется снимком, пользователь может её откатить. После правок вызовите run_checks.
Если в list_skills есть подходящий скилл, загрузите его через use_skill и следуйте ему.`

func (s *Server) version() string {
	if s.Version == "" {
		return "dev"
	}
	return s.Version
}

func (s *Server) skills() ([]skills.Skill, error) {
	list, err := skills.Load(skills.Dirs(s.Home, s.Root))
	if err != nil || s.Skills == nil {
		return list, err
	}
	allowed := map[string]bool{}
	for _, n := range s.Skills {
		allowed[n] = true
	}
	out := []skills.Skill{}
	for _, sk := range list {
		if allowed[sk.Name] {
			out = append(out, sk)
		}
	}
	return out, nil
}

type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations map[string]any  `json:"annotations,omitempty"`
}

var readOnly = map[string]bool{"project_skeleton": true, "search_code": true, "list_files": true, "read_file": true, "list_skills": true, "use_skill": true, "project_memory": true}

// noFiles — инструменты, которые не трогают файлы проекта: им не нужны
// блокировка и снимок.
var noFiles = map[string]bool{"remember": true}

func (s *Server) tools() []tool {
	// use_skill есть всегда: скиллы могут появиться, пока клиент подключён.
	ws := agent.Workspace{Root: s.Root, Skills: []skills.Skill{{}}, Memory: s.Memory}
	var out []tool
	for _, d := range ws.Tools() {
		if d.Function.Name == "finish" {
			continue
		}
		out = append(out, tool{Name: d.Function.Name, Description: d.Function.Description, InputSchema: d.Function.Parameters})
	}
	out = append(out,
		tool{Name: "run_checks", Description: "Запустить проверки проекта (линтеры и тесты: go vet/go test, ruff/pytest или .edy/hooks.json) и вернуть отчёт.", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
		tool{Name: "list_skills", Description: "Список скиллов — инструкций этого проекта и общих скиллов Edy.", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
	)
	for i := range out {
		out[i].Annotations = map[string]any{"readOnlyHint": readOnly[out[i].Name]}
	}
	return out
}

// call выполняет инструмент и возвращает текст и признак ошибки.
func (s *Server) call(ctx context.Context, name string, args json.RawMessage) (string, bool) {
	argStr := string(args)
	if argStr == "null" {
		argStr = ""
	}
	switch name {
	case "list_skills":
		list, err := s.skills()
		if err != nil {
			return "Ошибка: " + err.Error(), true
		}
		if len(list) == 0 {
			if s.Skills != nil {
				return "Для этой задачи скиллы не выданы.", false
			}
			return "Скиллов нет. Их можно добавить в Edy (раздел «Проект» → «Скиллы») или в папку " + skills.ProjectDir + "/<имя>/SKILL.md.", false
		}
		return skills.Catalog(list), false
	case "run_checks":
		hooks, err := agent.DetectHooks(s.Root)
		if err != nil {
			return "Ошибка: " + err.Error(), true
		}
		if len(hooks) == 0 {
			return "Проверки не найдены: нет go.mod, ruff/pytest или " + agent.HooksFile + ".", false
		}
		ok, report := agent.RunHooks(ctx, s.Root, hooks)
		return strings.TrimSpace(report), !ok
	}

	writes := !readOnly[name] && !noFiles[name]
	if writes {
		if s.Busy != nil && s.Busy() {
			return "Ошибка: над проектом сейчас работает задача Edy — правки через MCP временно запрещены.", true
		}
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	list, _ := s.skills()
	ws := agent.Workspace{Root: s.Root, Skills: list, Memory: s.Memory}
	out, changed, err := ws.Call(name, argStr)
	if err != nil {
		return "Ошибка: " + err.Error(), true
	}
	if changed {
		step := fmt.Sprintf("MCP: %s %s", name, pathArg(argStr))
		if err := s.snapshot(ctx, step); err != nil {
			out += "\n(снимок не сохранён: " + err.Error() + ")"
		} else if s.OnChange != nil {
			s.OnChange(step)
		}
	}
	return out, false
}

func (s *Server) snapshot(ctx context.Context, msg string) error {
	if s.shadow == nil {
		sh, err := agent.OpenShadow(ctx, s.Root)
		if err != nil {
			return err
		}
		s.shadow = &sh
	}
	_, err := s.shadow.Snapshot(ctx, msg)
	return err
}

func pathArg(args string) string {
	var a struct {
		Path string `json:"path"`
	}
	json.Unmarshal([]byte(args), &a)
	return a.Path
}

// ServeStdio читает сообщения построчно из in и пишет ответы в out
// (транспорт stdio: одно JSON-сообщение на строку).
func (s *Server) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	r := bufio.NewReaderSize(in, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if resp := s.Handle(ctx, bytes.TrimSpace(line)); resp != nil {
				if _, werr := out.Write(append(resp, '\n')); werr != nil {
					return werr
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
	}
}

// note — короткая запись о вызове для журнала: путь или запрос и первая строка ответа.
func note(args json.RawMessage, result string) string {
	var a struct {
		Path  string `json:"path"`
		Query string `json:"query"`
		Name  string `json:"name"`
	}
	json.Unmarshal(args, &a)
	what := strings.TrimSpace(a.Path + " " + a.Query + " " + a.Name)
	first, _, _ := strings.Cut(strings.TrimSpace(result), "\n")
	if r := []rune(first); len(r) > 160 {
		first = string(r[:160]) + "…"
	}
	if what == "" {
		return "→ " + first
	}
	return what + " → " + first
}
