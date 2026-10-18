package semantic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/laurenwilcox/name/internal/codeindex"
	"github.com/laurenwilcox/name/internal/store"
	"github.com/laurenwilcox/name/internal/vault"
)

// fakeEmb — «модель» из трёх понятий: доступ, арифметика, прочее.
type fakeEmb struct {
	mu    sync.Mutex
	texts int
	fail  bool
}

func concept(t string) []float32 {
	t = strings.ToLower(t)
	v := []float32{0, 0, 0.1}
	for _, w := range []string{"пароль", "credential", "secret", "логин"} {
		if strings.Contains(t, w) {
			v[0]++
		}
	}
	for _, w := range []string{"сумм", "add", "сложи"} {
		if strings.Contains(t, w) {
			v[1]++
		}
	}
	return v
}

func (f *fakeEmb) Embed(_ context.Context, model string, texts []string) ([][]float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errors.New("нет связи")
	}
	f.texts += len(texts)
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = concept(t)
	}
	return out, nil
}

func setup(t *testing.T) (*Engine, *fakeEmb, *store.Store, string) {
	t.Helper()
	v, _ := vault.New(bytes.Repeat([]byte{5}, 32))
	st, err := store.Open(filepath.Join(t.TempDir(), "edy.db"), v)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	root := t.TempDir()
	write(t, root, "auth.go", "package p\n\nfunc checkCredentials(user, secret string) bool {\n\treturn secret == \"x\"\n}\n")
	write(t, root, "math.go", "package p\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n")
	f := &fakeEmb{}
	e := New(f, st)
	return e, f, st, root
}

func write(t *testing.T, root, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIndexIncrementalAndSearch(t *testing.T) {
	e, f, st, root := setup(t)
	if _, err := e.Index(context.Background(), root, root); err == nil {
		t.Fatal("без модели индекс строиться не должен")
	}
	if err := e.SetModel("fake"); err == nil {
		t.Fatal("модель без провайдера принята")
	}
	if err := e.SetModel("ollama/fake"); err != nil {
		t.Fatal(err)
	}
	st.AddNote(store.Note{Project: root, Kind: store.NoteFact, Text: "Логин пользователя проверяется в auth.go"})
	st.AddNote(store.Note{Project: root, Kind: store.NoteFact, Text: "Тесты запускаются make test"})

	s, err := e.Index(context.Background(), root, root)
	if err != nil {
		t.Fatal(err)
	}
	if s.Total != 4 || s.Embedded != 4 || f.texts != 4 {
		t.Fatalf("первый индекс: %+v, текстов %d", s, f.texts)
	}
	s, _ = e.Index(context.Background(), root, root)
	if s.Embedded != 0 || f.texts != 4 {
		t.Fatalf("без изменений ничего не пересчитывается: %+v, текстов %d", s, f.texts)
	}
	write(t, root, "math.go", "package p\n\nfunc Add(a, b int) int {\n\treturn b + a\n}\n")
	s, _ = e.Index(context.Background(), root, root)
	if s.Embedded != 1 || f.texts != 5 {
		t.Fatalf("изменён один кусок: %+v, текстов %d", s, f.texts)
	}
	if list, _ := st.Embeddings(root, "ollama/fake"); len(list) != 4 {
		t.Fatalf("в базе %d векторов, а нужно 4 (старый кусок удалён)", len(list))
	}

	idx, err := codeindex.Build(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if bm := idx.Search("где проверяется пароль", 2); len(bm) != 0 {
		t.Fatalf("по словам «пароль» в коде не найти: %+v", bm)
	}
	hits, note := Code{E: e, Project: root}.Search(idx, "где проверяется пароль", 2)
	if note != "" || len(hits) == 0 || hits[0].Symbol != "checkCredentials" {
		t.Fatalf("по смыслу: %+v %q", hits, note)
	}

	notes, _ := st.Notes(root)
	got := Notes{E: e, Project: root}.Rank("куда вводят пароль", notes, 1)
	if len(got) != 1 || !strings.Contains(got[0].Text, "Логин") {
		t.Fatalf("заметки по смыслу: %+v", got)
	}

	// Модель недоступна — остаётся поиск по словам и пояснение.
	f.fail = true
	hits, note = Code{E: e, Project: root}.Search(idx, "Add", 2)
	if !strings.Contains(note, "нет связи") || len(hits) == 0 || hits[0].Symbol != "Add" {
		t.Fatalf("без модели: %+v %q", hits, note)
	}
	// Ошибка индексации не стирает уже готовые векторы.
	write(t, root, "auth.go", "package p\n\nfunc checkCredentials() bool { return true }\n")
	if _, err := e.Index(context.Background(), root, root); err == nil {
		t.Fatal("ошибка эмбеддингов потерялась")
	}
	if list, _ := st.Embeddings(root, "ollama/fake"); len(list) != 4 {
		t.Fatalf("после ошибки в базе %d векторов", len(list))
	}
	if e.Status(root).Error == "" {
		t.Fatal("ошибка не попала в статус")
	}

	// Модель выключена — только слова, без пояснений.
	e.SetModel("")
	hits, note = Code{E: e, Project: root}.Search(idx, "Add", 2)
	if note != "" || len(hits) == 0 {
		t.Fatalf("выключено: %+v %q", hits, note)
	}
}

func TestFuse(t *testing.T) {
	got := Fuse(3, []string{"a", "b", "c"}, []string{"c", "d"})
	if len(got) != 3 || got[0] != "c" {
		t.Fatalf("%v", got)
	}
}

func TestClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" || r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("X-Edy-Client") != "embeddings" {
			http.Error(w, `{"error":{"message":"плохой запрос"}}`, http.StatusBadRequest)
			return
		}
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Model == "bad/model" {
			http.Error(w, `{"error":{"message":"нет такой модели"}}`, http.StatusNotFound)
			return
		}
		// Ответ в обратном порядке: клиент раскладывает по index.
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"index": 1, "embedding": []float32{0, 1}},
			{"index": 0, "embedding": []float32{1, 0}},
		}})
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL + "/v1", Token: "tok", Client: "embeddings"}
	vecs, err := c.Embed(context.Background(), "p/m", []string{"a", "b"})
	if err != nil || vecs[0][0] != 1 || vecs[1][1] != 1 {
		t.Fatalf("%v %v", vecs, err)
	}
	if _, err := c.Embed(context.Background(), "bad/model", []string{"a"}); err == nil || !strings.Contains(err.Error(), "нет такой модели") {
		t.Fatalf("ошибка: %v", err)
	}
	if _, err := c.Embed(context.Background(), "p/m", []string{"a", "b", "c"}); err == nil {
		t.Fatal("неполный ответ принят")
	}
}
