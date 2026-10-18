package omniroute

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPAttachRules(t *testing.T) {
	if !SupportsMCP([]string{"/usr/local/bin/claude", "-p"}) || SupportsMCP([]string{"agy", "-p"}) || SupportsMCP(nil) {
		t.Fatal("MCP из аргументов умеет только Claude Code")
	}
	for raw, ok := range map[string]bool{
		"http://127.0.0.1:11435/mcp?project=/x": true,
		"http://localhost:1/mcp":                true,
		"https://evil.example/mcp":              false,
		"http://10.0.0.5:11435/mcp":             false,
		"http://127.0.0.1:11435/v1":             false,
	} {
		if err := checkMCPURL(raw); (err == nil) != ok {
			t.Errorf("%s: err = %v", raw, err)
		}
	}
	args, cleanup, err := claudeMCP(MCPAttach{URL: "http://127.0.0.1:1/mcp", Token: "edy-secret"})
	if err != nil {
		t.Fatal(err)
	}
	path := args[1]
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("конфиг с токеном должен быть доступен только владельцу: %v %v", info, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"Authorization":"Bearer edy-secret"`) {
		t.Fatalf("конфиг: %s", data)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("конфиг не удалён")
	}
}

func TestMaterializeImages(t *testing.T) {
	png := "data:image/png;base64,iVBORw0KGgo="
	content, _ := json.Marshal([]ContentPart{{Type: "text", Text: "Что на скриншоте?"}, {Type: "image_url", ImageURL: &ImageURL{URL: png}}, {Type: "image_url", ImageURL: &ImageURL{URL: "https://x/y.png"}}})
	msgs, dir, cleanup, err := materializeImages([]Message{{Role: "user", Content: content}})
	if err != nil || dir == "" {
		t.Fatalf("err=%v dir=%q", err, dir)
	}
	text, _ := msgs[0].Text()
	if !strings.Contains(text, "Что на скриншоте?") || !strings.Contains(text, "[Изображение: "+dir) || !strings.Contains(text, "[Изображение по ссылке: https://x/y.png]") {
		t.Fatalf("текст: %s", text)
	}
	data, err := os.ReadFile(filepath.Join(dir, "image-1.png"))
	if err != nil || len(data) != 8 {
		t.Fatalf("файл: %v %d", err, len(data))
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("папка с картинками должна удаляться")
	}
	audio, _ := json.Marshal([]ContentPart{{Type: "input_audio", InputAudio: &InputAudio{Data: "x", Format: "wav"}}})
	if _, _, _, err := materializeImages([]Message{{Role: "user", Content: audio}}); err == nil {
		t.Fatal("звук CLI не принимает")
	}
	plain, _ := json.Marshal("просто текст")
	if _, dir, _, _ := materializeImages([]Message{{Role: "user", Content: plain}}); dir != "" {
		t.Fatal("без картинок папка не создаётся")
	}
}
