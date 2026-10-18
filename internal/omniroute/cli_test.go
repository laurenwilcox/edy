package omniroute

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/laurenwilcox/name/internal/store"
	"github.com/laurenwilcox/name/internal/webchat"
)

// fakeCLI ведёт себя как терминальный агент: проверяет, что stdout — терминал,
// рисует спиннер и прогресс-бар, печатает ответ цветом.
const fakeCLI = `#!/bin/sh
model="$1"; prompt="$2"
if [ ! -t 1 ]; then echo "stdout не терминал"; exit 7; fi
case "$prompt" in
  fail*) echo "model not found"; exit 3 ;;
  sleep*) printf 'Trust this folder? (y/n) '; sleep 30 ;;
esac
printf '⠋ loading\r⠙ loading 50%%\r\033[2K'
printf '\033[1;32m[%s]\033[0m ответ на: %s\n' "$model" "$prompt"
printf 'cwd=%s\n' "$(pwd)"
`

func setupCLI(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-agent")
	if err := os.WriteFile(script, []byte(fakeCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	st := newTestStore(t)
	if _, err := st.AddProvider(store.Provider{Name: "local", Kind: store.KindCLI, Command: []string{script, "{model}", "{prompt}"}}); err != nil {
		t.Fatal(err)
	}
	return NewServer(st, testToken, nil), dir
}

func TestCLIAdapterThroughServer(t *testing.T) {
	srv, _ := setupCLI(t)
	workdir := t.TempDir()
	req := `{"model":"local/llama3","messages":[{"role":"user","content":"привет"}]}`
	r := newAuthedRequest(http.MethodPost, "/v1/chat/completions", req)
	r.Header.Set(WorkdirHeader, workdir)
	rec := serve(srv, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body)
	}
	var c Completion
	json.Unmarshal(rec.Body.Bytes(), &c)
	want := "[llama3] ответ на: привет\ncwd=" + resolve(workdir) + "\n"
	if got := *c.Choices[0].Message.Content; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
	if c.Usage.CompletionTokens == 0 {
		t.Fatal("ожидалась оценка токенов")
	}
}

func TestCLIAdapterFailure(t *testing.T) {
	srv, _ := setupCLI(t)
	rec := serve(srv, newAuthedRequest(http.MethodPost, "/v1/chat/completions", `{"model":"local/x","messages":[{"role":"user","content":"fail please"}]}`))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "model not found") {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body)
	}
}

func TestCLIAdapterRejectsImages(t *testing.T) {
	srv, _ := setupCLI(t)
	for _, body := range []string{
		`{"model":"local/x","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}]}`,
	} {
		if rec := serve(srv, newAuthedRequest(http.MethodPost, "/v1/chat/completions", body)); rec.Code != http.StatusBadRequest {
			t.Errorf("code = %d body = %s", rec.Code, rec.Body)
		}
	}
}

func TestCLIAdapterKilledOnCancel(t *testing.T) {
	_, dir := setupCLI(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	call := Call{
		Provider: store.Provider{Command: []string{filepath.Join(dir, "fake-agent"), "{model}", "{prompt}"}},
		Model:    "m",
		Request:  &ChatRequest{Messages: []Message{{Role: "user", Content: json.RawMessage(`"sleep"`)}}},
	}
	start := time.Now()
	_, err := (&CLIAdapter{}).Stream(ctx, call, func(Event) error { return nil })
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("err = %v, прошло %v", err, time.Since(start))
	}
	// В ошибке должен быть вопрос, на котором утилита зависла.
	if !strings.Contains(err.Error(), "Trust this folder? (y/n)") {
		t.Fatalf("err = %v", err)
	}
}

func TestRenderPrompt(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: json.RawMessage(`"Отвечай кратко"`)},
		{Role: "user", Content: json.RawMessage(`"2+2?"`)},
	}
	got, err := renderPrompt(msgs)
	if err != nil {
		t.Fatal(err)
	}
	want := "### Инструкции\nОтвечай кратко\n\n### Пользователь\n2+2?\n\n### Ассистент\n"
	if got != want {
		t.Fatalf("got %q", got)
	}
}

func resolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func TestAntigravityPresetKeepsModelWithSpaces(t *testing.T) {
	args, err := expandCommand(CLIPresets["antigravity"].Command, "Claude Opus 5.5 (Thinking)", "привет")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"agy", "-p", "привет", "--model", "Claude Opus 5.5 (Thinking)"}
	if strings.Join(args, "|") != strings.Join(want, "|") {
		t.Fatalf("args = %q", args)
	}
}

func TestFindExecutableInExtraDirs(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "edy-test-tool")
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "not-exec"), []byte("x"), 0o644)

	got, err := findExecutable("edy-test-tool", []string{"/nonexistent", dir})
	if err != nil || got != bin {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := findExecutable("not-exec", []string{dir}); err == nil {
		t.Fatal("неисполняемый файл не должен находиться")
	}
	if got, err := findExecutable("sh", nil); err != nil || !filepath.IsAbs(got) {
		t.Fatalf("утилита из PATH: %q, %v", got, err)
	}
}

func TestPresetsHaveInstallAndPrompt(t *testing.T) {
	for name, p := range CLIPresets {
		if p.Install == "" {
			t.Errorf("%s: нет команды установки", name)
		}
		if _, err := expandCommand(p.Command, "m", "p"); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestAPIPresetsAreConsistent(t *testing.T) {
	for name, p := range APIPresets {
		if p.Kind != store.KindOpenAI && p.Kind != store.KindAnthropic {
			t.Errorf("%s: неизвестный тип %q", name, p.Kind)
		}
		if p.Kind == store.KindOpenAI && !strings.HasPrefix(p.BaseURL, "http") {
			t.Errorf("%s: нет адреса API", name)
		}
	}
}

type fakeBrowser struct{ chunks []string }

func (f fakeBrowser) Ask(ctx context.Context, c webchat.Conn, prompt string, images []string, onText func(string) error) error {
	if c.Profile != "gpt" || c.Site != "chatgpt" || c.Browser != "chromium" {
		return fmt.Errorf("подключение = %+v", c)
	}
	full := ""
	for _, c := range f.chunks {
		full += c
		if err := onText(full); err != nil {
			return err
		}
	}
	return nil
}

func TestWebAdapterThroughServer(t *testing.T) {
	st := newTestStore(t)
	st.AddProvider(store.Provider{Name: "gpt", Kind: store.KindWeb, Site: "chatgpt", Browser: "chromium"})
	srv := NewServer(st, testToken, nil)
	srv.RegisterAdapter(store.KindWeb, &WebAdapter{Browser: fakeBrowser{chunks: []string{"При", "вет", "!"}}})
	rec := serve(srv, newAuthedRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gpt/default","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `"content":"вет"`) || !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("code = %d body = %s", rec.Code, body)
	}
}

// imageBrowser запоминает, что адаптер передал веб-чату.
type imageBrowser struct {
	prompt string
	sizes  []int
}

func (b *imageBrowser) Ask(ctx context.Context, c webchat.Conn, prompt string, images []string, onText func(string) error) error {
	b.prompt = prompt
	for _, p := range images {
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		b.sizes = append(b.sizes, len(data))
	}
	return onText("Кот. ![image](data:image/png;base64,iVBORw0KGgo=)")
}

func TestWebAdapterPassesImages(t *testing.T) {
	st := newTestStore(t)
	st.AddProvider(store.Provider{Name: "gpt", Kind: store.KindWeb, Site: "chatgpt", Browser: "chromium"})
	srv := NewServer(st, testToken, nil)
	b := &imageBrowser{}
	srv.RegisterAdapter(store.KindWeb, &WebAdapter{Browser: b})
	body := `{"model":"gpt/default","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"Кто это?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]}`
	rec := serve(srv, newAuthedRequest(http.MethodPost, "/v1/chat/completions", body))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `![image](data:image/png;base64,iVBORw0KGgo=)`) {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(b.prompt, "Кто это?") || !strings.Contains(b.prompt, "[Изображение 1 — во вложении]") || len(b.sizes) != 1 || b.sizes[0] != 8 {
		t.Fatalf("prompt = %q sizes = %v", b.prompt, b.sizes)
	}
}
