// Package memory — память проекта: поиск по заметкам и короткая выжимка
// для промптов. Чтобы не раздувать расход токенов, в промпт попадают только
// закреплённые заметки и те, что похожи на текущую задачу, в пределах
// бюджета; остальное агент ищет сам инструментом project_memory.
package memory

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/laurenwilcox/name/internal/store"
)

// MaxNote — длина одной заметки: память — не место для простыней.
const MaxNote = 400

// Store — хранилище заметок.
type Store interface {
	AddNote(n store.Note) (int64, error)
	Notes(project string) ([]store.Note, error)
}

var kindTitle = map[string]string{store.NoteFact: "факт", store.NoteLesson: "урок", store.NoteTask: "задача", store.NoteNote: "заметка"}

// terms — слова для поиска: нижний регистр и грубая основа (первые 4–5
// букв), чтобы «тесты» и «тестов», «возврат» и «возвраты» совпадали.
func terms(s string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		r := []rune(w)
		if len(r) < 2 {
			continue
		}
		switch {
		case len(r) >= 7:
			r = r[:5]
		case len(r) >= 4:
			r = r[:4]
		}
		out = append(out, string(r))
	}
	return out
}

// Search возвращает до n заметок, похожих на запрос (BM25).
func Search(notes []store.Note, query string, n int) []store.Note {
	q := terms(query)
	if len(q) == 0 || len(notes) == 0 {
		return nil
	}
	docs := make([][]string, len(notes))
	df := map[string]int{}
	total := 0
	for i, nt := range notes {
		docs[i] = terms(nt.Text)
		total += len(docs[i])
		seen := map[string]bool{}
		for _, t := range docs[i] {
			if !seen[t] {
				seen[t] = true
				df[t]++
			}
		}
	}
	avg := float64(total) / float64(len(notes))
	type hit struct {
		i     int
		score float64
	}
	var hits []hit
	for i, d := range docs {
		tf := map[string]int{}
		for _, t := range d {
			tf[t]++
		}
		score := 0.0
		for _, t := range q {
			if tf[t] == 0 {
				continue
			}
			idf := math.Log(1 + (float64(len(notes))-float64(df[t])+0.5)/(float64(df[t])+0.5))
			f := float64(tf[t])
			score += idf * f * 2.2 / (f + 1.2*(0.25+0.75*float64(len(d))/avg))
		}
		if score > 0 {
			hits = append(hits, hit{i, score})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].score > hits[b].score })
	var out []store.Note
	for _, h := range hits {
		if len(out) == n {
			break
		}
		out = append(out, notes[h.i])
	}
	return out
}

func line(n store.Note) string {
	return fmt.Sprintf("- [%s] %s\n", kindTitle[n.Kind], strings.ReplaceAll(strings.TrimSpace(n.Text), "\n", " "))
}

// Brief — выжимка для промпта: закреплённые заметки, затем похожие на
// запрос, не длиннее budget символов. Пусто — памяти нет.
func Brief(notes []store.Note, query string, budget int) string {
	var b strings.Builder
	used := map[int64]bool{}
	add := func(n store.Note) bool {
		l := line(n)
		if used[n.ID] {
			return true
		}
		if b.Len()+len(l) > budget {
			return false
		}
		used[n.ID] = true
		b.WriteString(l)
		return true
	}
	for _, n := range notes {
		if n.Pinned && !add(n) {
			break
		}
	}
	if query != "" {
		for _, n := range Search(notes, query, 8) {
			if !add(n) {
				break
			}
		}
	}
	return b.String()
}

// Pinned — только закреплённые заметки (для исполнителя).
func Pinned(notes []store.Note, budget int) string {
	var pinned []store.Note
	for _, n := range notes {
		if n.Pinned && n.Agent == "" {
			pinned = append(pinned, n)
		}
	}
	return Brief(pinned, "", budget)
}

// AgentBrief — выжимка для постоянного агента: его личные заметки
// (закреплённые и похожие на задачу) и закреплённое общее, в пределах budget.
func AgentBrief(notes []store.Note, agentID, goal string, budget int) string {
	var own, shared []store.Note
	for _, n := range notes {
		switch {
		case n.Agent == agentID:
			own = append(own, n)
		case n.Agent == "" && n.Pinned:
			shared = append(shared, n)
		}
	}
	var b strings.Builder
	if mine := Brief(own, goal, budget*2/3); mine != "" {
		b.WriteString("Твоя память (опыт прошлых задач):\n" + mine)
	}
	if rest := Brief(shared, "", budget-b.Len()); rest != "" {
		b.WriteString("Общее закреплённое:\n" + rest)
	}
	return b.String()
}

// Access — доступ агента к памяти одного проекта (инструменты
// project_memory и remember). Записать агент может не больше нескольких
// заметок за задачу.
type Access struct {
	St      Store
	Project string
	Source  string
	Agent   string // постоянный агент: его заметки попадают в личную память
	Limit   int    // сколько заметок можно записать; 0 — 5

	mu      sync.Mutex
	written int
}

// Search ищет в памяти проекта.
func (a *Access) Search(query string) string {
	notes, err := a.St.Notes(a.Project)
	if err != nil {
		return "Ошибка памяти проекта: " + err.Error()
	}
	if len(notes) == 0 {
		return "Память проекта пуста."
	}
	hits := Search(notes, query, 8)
	if len(hits) == 0 {
		return "В памяти проекта ничего похожего не нашлось."
	}
	var b strings.Builder
	for _, n := range hits {
		b.WriteString(line(n))
	}
	return b.String()
}

// Remember записывает факт о проекте.
func (a *Access) Remember(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("пустая заметка")
	}
	if len([]rune(text)) > MaxNote {
		return fmt.Errorf("заметка длиннее %d символов — запишите короче", MaxNote)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	limit := a.Limit
	if limit == 0 {
		limit = 5
	}
	if a.written >= limit {
		return fmt.Errorf("за одну задачу можно записать не больше %d заметок", limit)
	}
	if _, err := a.St.AddNote(store.Note{Project: a.Project, Kind: store.NoteFact, Text: text, Source: a.Source, Agent: a.Agent}); err != nil {
		return err
	}
	a.written++
	return nil
}
