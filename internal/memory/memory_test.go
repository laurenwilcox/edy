package memory

import (
	"strings"
	"testing"

	"github.com/laurenwilcox/name/internal/store"
)

type mem struct{ notes []store.Note }

func (m *mem) AddNote(n store.Note) (int64, error) {
	n.ID = int64(len(m.notes) + 1)
	m.notes = append([]store.Note{n}, m.notes...)
	return n.ID, nil
}
func (m *mem) Notes(string) ([]store.Note, error) { return m.notes, nil }

func TestSearchAndBrief(t *testing.T) {
	notes := []store.Note{
		{ID: 1, Kind: store.NoteFact, Text: "Тесты запускаются через make test, нужен docker", Pinned: true},
		{ID: 2, Kind: store.NoteLesson, Text: "Модуль оплаты хрупкий: не трогать без тестов на возвраты"},
		{ID: 3, Kind: store.NoteTask, Text: "Добавлен экспорт отчёта в CSV"},
		{ID: 4, Kind: store.NoteNote, Text: "Логи пишутся в /var/log/app"},
	}
	hits := Search(notes, "почини возврат в оплате", 3)
	if len(hits) == 0 || hits[0].ID != 2 {
		t.Fatalf("поиск: %+v", hits)
	}
	if got := Search(notes, "тестов", 5); len(got) != 2 {
		t.Fatalf("основа слова: %+v", got)
	}
	b := Brief(notes, "оплата", 2000)
	if !strings.HasPrefix(b, "- [факт] Тесты запускаются") || !strings.Contains(b, "[урок] Модуль оплаты") || strings.Contains(b, "CSV") {
		t.Fatalf("выжимка: закреплённое + похожее, без лишнего:\n%s", b)
	}
	if b := Brief(notes, "оплата", 60); strings.Contains(b, "оплаты") {
		t.Fatalf("бюджет: %q", b)
	}
	if p := Pinned(notes, 500); strings.Count(p, "\n") != 1 {
		t.Fatalf("закреплённые: %q", p)
	}
}

func TestAccess(t *testing.T) {
	m := &mem{}
	a := &Access{St: m, Project: "/p", Source: "task:1", Limit: 2}
	if out := a.Search("x"); !strings.Contains(out, "пуста") {
		t.Fatal(out)
	}
	if err := a.Remember(strings.Repeat("я", MaxNote+1)); err == nil {
		t.Fatal("слишком длинная заметка")
	}
	a.Remember("Сборка: go build ./cmd/app")
	a.Remember("Конфиг в config.yaml")
	if err := a.Remember("третья"); err == nil {
		t.Fatal("лимит заметок за задачу")
	}
	if out := a.Search("сборка"); !strings.Contains(out, "go build") {
		t.Fatalf("поиск по записанному: %s", out)
	}
	if m.notes[0].Source != "task:1" || m.notes[0].Kind != store.NoteFact {
		t.Fatalf("заметка: %+v", m.notes[0])
	}
}
