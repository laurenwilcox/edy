package omniroute

import (
	"path/filepath"

	"github.com/laurenwilcox/name/internal/store"
)

// Возможности провайдеров: по ним Оркестратор решает, что можно выдать
// исполнителю (MCP, скиллы) и какие задачи ему вообще по силам.
const (
	CapMCP         = "mcp"          // подключает MCP-серверы (Edy и внешние)
	CapTools       = "tools"        // родной вызов функций
	CapVision      = "vision"       // понимает картинки
	CapAudio       = "audio"        // понимает звук и речь
	CapVoice       = "voice"        // отвечает голосом
	CapImageGen    = "image_gen"    // рисует картинки
	CapWeb         = "web"          // ищет в интернете
	CapLongContext = "long_context" // длинный контекст (сотни тысяч токенов)
)

// CapInfo — описание возможности для интерфейса и для Оркестратора.
type CapInfo struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Hint  string `json:"hint"`
}

// CapCatalog — все возможности в порядке показа.
var CapCatalog = []CapInfo{
	{CapMCP, "MCP", "подключает MCP Edy и внешние MCP-серверы; скиллы получает через MCP"},
	{CapTools, "Инструменты", "родной вызов функций (без эмуляции в промпте)"},
	{CapVision, "Картинки", "понимает изображения и скриншоты"},
	{CapAudio, "Звук", "понимает аудио и речь"},
	{CapVoice, "Голос", "отвечает голосом"},
	{CapImageGen, "Рисует", "создаёт изображения"},
	{CapWeb, "Интернет", "сам ищет в интернете"},
	{CapLongContext, "Длинный контекст", "держит в голове большой проект целиком"},
}

// CapTitle — человекочитаемое имя возможности.
func CapTitle(id string) string {
	for _, c := range CapCatalog {
		if c.ID == id {
			return c.Title
		}
	}
	return id
}

// DefaultCaps — возможности по типу провайдера, пока пользователь не задал свои.
// Учитывается то, что работает через Edy: веб-чаты, например, передают
// только текст, поэтому картинок у них нет.
func DefaultCaps(p store.Provider) []string {
	switch p.Kind {
	case store.KindAnthropic:
		return []string{CapTools, CapVision, CapLongContext}
	case store.KindOpenAI:
		return []string{CapTools}
	case store.KindCLI:
		if len(p.Command) == 0 {
			return []string{}
		}
		switch filepath.Base(p.Command[0]) {
		case "claude":
			return []string{CapMCP, CapTools, CapVision, CapWeb, CapLongContext}
		case "agy", "gemini", "qwen":
			return []string{CapTools, CapWeb, CapLongContext}
		case "codex", "copilot", "opencode", "cursor-agent", "agent":
			return []string{CapTools, CapWeb}
		}
		return []string{}
	case store.KindWeb:
		return []string{CapWeb}
	}
	return []string{}
}

// Caps — действующие возможности провайдера. MCP остаётся, только если
// утилита технически умеет его подключать: включить его вручную нельзя.
func Caps(p store.Provider) []string {
	caps := p.Caps
	if caps == nil {
		caps = DefaultCaps(p)
	}
	mcp := p.Kind == store.KindCLI && SupportsMCP(p.Command)
	out := []string{}
	seen := map[string]bool{}
	for _, c := range caps {
		if seen[c] || (c == CapMCP && !mcp) || CapTitle(c) == c {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

// HasCap сообщает, есть ли у провайдера возможность.
func HasCap(p store.Provider, c string) bool {
	for _, x := range Caps(p) {
		if x == c {
			return true
		}
	}
	return false
}
