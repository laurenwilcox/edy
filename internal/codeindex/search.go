package codeindex

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Chunk — кусок кода для поиска: одна функция, тип, класс или блок строк.
type Chunk struct {
	Path   string
	Symbol string
	Start  int
	End    int
	Text   string
	terms  map[string]int
	length int
}

// Hit — найденный кусок.
type Hit struct {
	Path   string  `json:"path"`
	Symbol string  `json:"symbol"`
	Start  int     `json:"start"`
	End    int     `json:"end"`
	Score  float64 `json:"score"`
	Text   string  `json:"text"`
}

var wordRe = regexp.MustCompile(`[\p{L}\p{N}_]+`)

// tokenize режет текст на слова и разбивает идентификаторы:
// parseRequest → parse, request, parserequest; user_name → user, name, user_name.
func tokenize(s string) []string {
	var out []string
	for _, w := range wordRe.FindAllString(s, -1) {
		lw := strings.ToLower(w)
		if len([]rune(lw)) < 2 {
			continue
		}
		out = append(out, lw)
		parts := splitIdent(w)
		if len(parts) > 1 {
			for _, p := range parts {
				if len([]rune(p)) >= 2 {
					out = append(out, strings.ToLower(p))
				}
			}
		}
	}
	return out
}

func splitIdent(w string) []string {
	var parts []string
	for _, seg := range strings.Split(w, "_") {
		start := 0
		r := []rune(seg)
		for i := 1; i < len(r); i++ {
			if unicode.IsUpper(r[i]) && (unicode.IsLower(r[i-1]) || (i+1 < len(r) && unicode.IsLower(r[i+1]))) {
				parts = append(parts, string(r[start:i]))
				start = i
			}
		}
		if start < len(r) {
			parts = append(parts, string(r[start:]))
		}
	}
	return parts
}

// Chunks режет проект на куски: по объявлениям для Go и Python, по строкам для остального.
func (idx *Index) Chunks() []Chunk {
	var chunks []Chunk
	for _, f := range idx.Files {
		data, err := os.ReadFile(filepath.Join(idx.Root, filepath.FromSlash(f.Path)))
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		add := func(sym string, start, end int) {
			if start < 1 {
				start = 1
			}
			if end > len(lines) {
				end = len(lines)
			}
			if start > end {
				return
			}
			text := strings.Join(lines[start-1:end], "\n")
			c := Chunk{Path: f.Path, Symbol: sym, Start: start, End: end, Text: text, terms: map[string]int{}}
			// Имя файла и символа тоже участвуют в поиске.
			for _, t := range tokenize(f.Path + " " + sym + " " + sym + " " + text) {
				c.terms[t]++
				c.length++
			}
			chunks = append(chunks, c)
		}
		covered := 0
		// Объявления верхнего уровня; вложенные методы классов покрываются классом.
		for _, s := range f.Symbols {
			if s.Line <= covered {
				continue
			}
			add(s.Name, s.Line, s.End)
			covered = s.End
		}
		if len(f.Symbols) == 0 {
			for start := 1; start <= len(lines); start += chunkLines {
				add("", start, start+chunkLines-1)
			}
		}
	}
	return chunks
}

// Search ищет куски кода по запросу (BM25) и возвращает лучшие k.
func (idx *Index) Search(query string, k int) []Hit {
	q := tokenize(query)
	if len(q) == 0 {
		return nil
	}
	chunks := idx.Chunks()
	if len(chunks) == 0 {
		return nil
	}
	df := map[string]int{}
	total := 0
	for _, c := range chunks {
		total += c.length
		for t := range c.terms {
			df[t]++
		}
	}
	avg := float64(total) / float64(len(chunks))
	const k1, b = 1.2, 0.75
	n := float64(len(chunks))
	var hits []Hit
	for _, c := range chunks {
		score := 0.0
		for _, t := range q {
			tf := float64(c.terms[t])
			if tf == 0 {
				continue
			}
			idf := math.Log(1 + (n-float64(df[t])+0.5)/(float64(df[t])+0.5))
			score += idf * tf * (k1 + 1) / (tf + k1*(1-b+b*float64(c.length)/avg))
		}
		if score > 0 {
			hits = append(hits, Hit{Path: c.Path, Symbol: c.Symbol, Start: c.Start, End: c.End, Score: score, Text: c.Text})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

// FormatHits — результат поиска для модели: путь, строки и код с номерами строк.
func FormatHits(hits []Hit, maxLines int) string {
	if len(hits) == 0 {
		return "Ничего не найдено. Попробуйте другие слова или посмотрите project_skeleton."
	}
	var b strings.Builder
	for _, h := range hits {
		title := h.Path
		if h.Symbol != "" {
			title += " · " + h.Symbol
		}
		fmt.Fprintf(&b, "── %s (строки %d–%d)\n", title, h.Start, h.End)
		lines := strings.Split(h.Text, "\n")
		for i, l := range lines {
			if i >= maxLines {
				fmt.Fprintf(&b, "   …ещё %d строк — read_file\n", len(lines)-maxLines)
				break
			}
			fmt.Fprintf(&b, "%5d  %s\n", h.Start+i, l)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
