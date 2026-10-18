// Package semantic — поиск по смыслу: куски кода и заметки памяти
// превращаются в векторы (эмбеддинги) через OmniRoute /v1/embeddings и
// хранятся в базе Edy. При обновлении индекса пересчитываются только
// изменённые куски (по хешу текста). Поиск гибридный: BM25 по словам и
// косинусная близость векторов объединяются (reciprocal rank fusion), так
// что «где проверяется пароль» находит checkCredentials, даже если слова
// «пароль» в коде нет.
package semantic

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/laurenwilcox/name/internal/codeindex"
	"github.com/laurenwilcox/name/internal/store"
)

// SettingModel — глобальная настройка: модель эмбеддингов «провайдер/модель».
// Пусто — поиск по смыслу выключен, работает только поиск по словам.
const SettingModel = "embed_model"

const (
	embedChars = 1500 // сколько символов куска уходит в эмбеддинг
	batchSize  = 64
)

// Embedder превращает тексты в векторы.
type Embedder interface {
	Embed(ctx context.Context, model string, texts []string) ([][]float32, error)
}

// Client — эмбеддинги через OmniRoute (формат OpenAI).
type Client struct {
	BaseURL string
	Token   string
	Client  string // X-Edy-Client для учёта расходов
	HTTP    *http.Client
}

func (c *Client) Embed(ctx context.Context, model string, texts []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": model, "input": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if c.Client != "" {
		req.Header.Set("X-Edy-Client", c.Client)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(data, &e)
		if e.Error.Message == "" {
			e.Error.Message = strings.TrimSpace(string(data))
		}
		return nil, fmt.Errorf("эмбеддинги %s: %s", model, e.Error.Message)
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("эмбеддинги %s: неожиданный ответ", model)
	}
	vecs := make([][]float32, len(texts))
	for i, d := range out.Data {
		idx := d.Index
		if idx < 0 || idx >= len(vecs) {
			idx = i
		}
		if idx < len(vecs) {
			vecs[idx] = d.Embedding
		}
	}
	for _, v := range vecs {
		if len(v) == 0 {
			return nil, fmt.Errorf("эмбеддинги %s: модель вернула не все векторы", model)
		}
	}
	return vecs, nil
}

// Store — где лежат векторы, заметки и настройки.
type Store interface {
	Embeddings(project, model string) ([]store.Embedding, error)
	SaveEmbeddings(project, model string, list []store.Embedding, keep map[string]bool) error
	Notes(project string) ([]store.Note, error)
	Setting(key string) (string, error)
	SetSetting(key, value string) error
}

// Status — состояние индекса проекта.
type Status struct {
	Model    string    `json:"model"`
	Indexing bool      `json:"indexing"`
	Done     int       `json:"done"`  // сколько кусков уже с векторами
	Total    int       `json:"total"` // сколько кусков всего
	Embedded int       `json:"embedded"`
	Error    string    `json:"error,omitempty"`
	Updated  time.Time `json:"updated,omitempty"`
}

// Engine — индекс и поиск по смыслу.
type Engine struct {
	Emb Embedder
	St  Store

	mu     sync.Mutex
	status map[string]*Status
	locks  map[string]*sync.Mutex
	cache  map[string]*vectors
}

type vectors struct {
	model  string
	byHash map[string][]float32
}

func New(emb Embedder, st Store) *Engine {
	return &Engine{Emb: emb, St: st, status: map[string]*Status{}, locks: map[string]*sync.Mutex{}, cache: map[string]*vectors{}}
}

// Model — модель эмбеддингов; пусто — поиск по смыслу выключен.
func (e *Engine) Model() string {
	if e == nil || e.St == nil {
		return ""
	}
	m, _ := e.St.Setting(SettingModel)
	return strings.TrimSpace(m)
}

// SetModel меняет модель. Старые векторы другой модели не годятся и
// заменятся при следующей индексации.
func (e *Engine) SetModel(model string) error {
	model = strings.TrimSpace(model)
	if model != "" && !strings.Contains(model, "/") {
		return errors.New("модель указывается как «провайдер/модель»")
	}
	if err := e.St.SetSetting(SettingModel, model); err != nil {
		return err
	}
	e.mu.Lock()
	e.cache = map[string]*vectors{}
	e.mu.Unlock()
	return nil
}

// Status — состояние индекса проекта.
func (e *Engine) Status(project string) Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s, ok := e.status[project]; ok {
		out := *s
		out.Model = e.Model()
		return out
	}
	return Status{Model: e.Model()}
}

func (e *Engine) setStatus(project string, f func(*Status)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.status[project]
	if !ok {
		s = &Status{}
		e.status[project] = s
	}
	f(s)
}

func (e *Engine) lock(project string) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	l, ok := e.locks[project]
	if !ok {
		l = &sync.Mutex{}
		e.locks[project] = l
	}
	return l
}

func hash(text string) string {
	h := sha1.Sum([]byte(text))
	return hex.EncodeToString(h[:])
}

// codeText — что уходит в эмбеддинг для куска кода.
func codeText(c codeindex.Chunk) string {
	t := c.Text
	if len(t) > embedChars {
		t = t[:embedChars]
		for len(t) > 0 && !utf8Start(t[len(t)-1]) {
			t = t[:len(t)-1]
		}
	}
	head := c.Path
	if c.Symbol != "" {
		head += " · " + c.Symbol
	}
	return head + "\n" + t
}

// utf8Start — байт начинает символ UTF-8 (не продолжение), чтобы не резать букву.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

func noteText(n store.Note) string { return n.Text }

type item struct {
	key, text, hash string
}

// Index строит или обновляет индекс проекта project (папка root): куски
// кода и заметки памяти. Векторы с тем же текстом и моделью переиспользуются.
func (e *Engine) Index(ctx context.Context, project, root string) (Status, error) {
	model := e.Model()
	if model == "" {
		return Status{}, errors.New("модель эмбеддингов не выбрана")
	}
	l := e.lock(project)
	l.Lock()
	defer l.Unlock()

	idx, err := codeindex.Build(ctx, root)
	if err != nil {
		return Status{}, err
	}
	var items []item
	for _, c := range idx.Chunks() {
		t := codeText(c)
		items = append(items, item{key: "code:" + c.Path + "#" + c.Symbol + "#" + strconv.Itoa(c.Start), text: t, hash: hash(t)})
	}
	if notes, err := e.St.Notes(project); err == nil {
		for _, n := range notes {
			t := noteText(n)
			items = append(items, item{key: "note:" + strconv.FormatInt(n.ID, 10), text: t, hash: hash(t)})
		}
	}
	old, err := e.St.Embeddings(project, model)
	if err != nil {
		return Status{}, err
	}
	byHash := map[string][]float32{}
	for _, o := range old {
		byHash[o.Hash] = o.Vec
	}
	keep := map[string]bool{}
	var save []store.Embedding
	var todo []item
	for _, it := range items {
		keep[it.key] = true
		if v, ok := byHash[it.hash]; ok {
			save = append(save, store.Embedding{Key: it.key, Hash: it.hash, Vec: v})
		} else {
			todo = append(todo, it)
		}
	}
	e.setStatus(project, func(s *Status) {
		s.Indexing, s.Error, s.Total, s.Done, s.Embedded = true, "", len(items), len(items)-len(todo), 0
	})
	defer e.setStatus(project, func(s *Status) { s.Indexing = false })

	embedded := 0
	var firstErr error
	for start := 0; start < len(todo); start += batchSize {
		end := min(start+batchSize, len(todo))
		texts := make([]string, end-start)
		for i, it := range todo[start:end] {
			texts[i] = it.text
		}
		vecs, err := e.Emb.Embed(ctx, model, texts)
		if err != nil {
			firstErr = err
			break
		}
		for i, it := range todo[start:end] {
			v := normalize(vecs[i])
			save = append(save, store.Embedding{Key: it.key, Hash: it.hash, Vec: v})
			byHash[it.hash] = v
		}
		embedded += end - start
		e.setStatus(project, func(s *Status) { s.Done += end - start; s.Embedded = embedded })
	}
	// Сохраняем и то, что успели: при следующем запуске догоним остальное.
	// Пока индекс неполный, старые векторы без пары не удаляем.
	if firstErr != nil {
		for _, o := range old {
			keep[o.Key] = true
		}
		saved := map[string]bool{}
		for _, s := range save {
			saved[s.Key] = true
		}
		for _, o := range old {
			if !saved[o.Key] {
				save = append(save, o)
			}
		}
	}
	if err := e.St.SaveEmbeddings(project, model, save, keep); err != nil {
		return Status{}, err
	}
	e.mu.Lock()
	delete(e.cache, project)
	e.mu.Unlock()
	now := time.Now()
	e.setStatus(project, func(s *Status) {
		s.Updated = now
		if firstErr != nil {
			s.Error = firstErr.Error()
		}
	})
	st := e.Status(project)
	st.Indexing = false
	return st, firstErr
}

// Start запускает Index в фоне, если индексация проекта ещё не идёт.
func (e *Engine) Start(project, root string, timeout time.Duration) {
	e.mu.Lock()
	s, ok := e.status[project]
	if !ok {
		s = &Status{}
		e.status[project] = s
	}
	if s.Indexing {
		e.mu.Unlock()
		return
	}
	s.Indexing, s.Error = true, ""
	e.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if _, err := e.Index(ctx, project, root); err != nil {
			e.setStatus(project, func(s *Status) { s.Error = err.Error() })
		}
	}()
}

func normalize(v []float32) []float32 {
	var sum float64
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	out := make([]float32, len(v))
	if sum == 0 {
		return out
	}
	n := float32(1 / math.Sqrt(sum))
	for i, f := range v {
		out[i] = f * n
	}
	return out
}

func dot(a, b []float32) float64 {
	if len(a) != len(b) {
		return -1
	}
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

// vectorsFor — векторы проекта по хешу текста (с кешем в памяти).
func (e *Engine) vectorsFor(project, model string) (map[string][]float32, error) {
	e.mu.Lock()
	if c, ok := e.cache[project]; ok && c.model == model {
		e.mu.Unlock()
		return c.byHash, nil
	}
	e.mu.Unlock()
	list, err := e.St.Embeddings(project, model)
	if err != nil {
		return nil, err
	}
	byHash := make(map[string][]float32, len(list))
	for _, x := range list {
		byHash[x.Hash] = x.Vec
	}
	e.mu.Lock()
	e.cache[project] = &vectors{model: model, byHash: byHash}
	e.mu.Unlock()
	return byHash, nil
}

// rank возвращает номера текстов, отсортированные по близости к запросу.
// Тексты без вектора (ещё не проиндексированы) не участвуют.
func (e *Engine) rank(ctx context.Context, project, query string, texts []string, k int) ([]int, error) {
	model := e.Model()
	if model == "" {
		return nil, nil
	}
	byHash, err := e.vectorsFor(project, model)
	if err != nil || len(byHash) == 0 {
		return nil, err
	}
	qv, err := e.Emb.Embed(ctx, model, []string{query})
	if err != nil {
		return nil, err
	}
	q := normalize(qv[0])
	type scored struct {
		i int
		s float64
	}
	var list []scored
	for i, t := range texts {
		if v, ok := byHash[hash(t)]; ok {
			list = append(list, scored{i, dot(q, v)})
		}
	}
	sort.SliceStable(list, func(a, b int) bool { return list[a].s > list[b].s })
	if len(list) > k {
		list = list[:k]
	}
	out := make([]int, len(list))
	for i, x := range list {
		out[i] = x.i
	}
	return out, nil
}

// Fuse объединяет ранжированные списки ключей (reciprocal rank fusion).
func Fuse(k int, lists ...[]string) []string {
	score := map[string]float64{}
	var order []string
	for _, l := range lists {
		for r, key := range l {
			if _, ok := score[key]; !ok {
				order = append(order, key)
			}
			score[key] += 1 / float64(60+r+1)
		}
	}
	sort.SliceStable(order, func(a, b int) bool { return score[order[a]] > score[order[b]] })
	if len(order) > k {
		order = order[:k]
	}
	return order
}

// Code — поиск кода проекта по смыслу для папки root (сам проект или его
// копия-worktree): реализует agent.CodeSearch.
type Code struct {
	E       *Engine
	Project string
	Timeout time.Duration // на один запрос эмбеддинга; 0 — 20 с
}

func chunkKey(c codeindex.Chunk) string { return c.Path + "#" + strconv.Itoa(c.Start) }

// Search — гибридный поиск: BM25 и смысл. Если эмбеддинги недоступны,
// возвращает результат BM25 и причину.
func (c Code) Search(idx *codeindex.Index, query string, k int) ([]codeindex.Hit, string) {
	bm := idx.Search(query, k*3)
	if c.E == nil || c.E.Model() == "" {
		return trim(bm, k), ""
	}
	chunks := idx.Chunks()
	texts := make([]string, len(chunks))
	for i, ch := range chunks {
		texts[i] = codeText(ch)
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ranked, err := c.E.rank(ctx, c.Project, query, texts, k*3)
	if err != nil {
		return trim(bm, k), "поиск по смыслу недоступен: " + err.Error()
	}
	if len(ranked) == 0 {
		return trim(bm, k), ""
	}
	byKey := map[string]codeindex.Hit{}
	var bmKeys, semKeys []string
	for _, h := range bm {
		key := h.Path + "#" + strconv.Itoa(h.Start)
		byKey[key] = h
		bmKeys = append(bmKeys, key)
	}
	for _, i := range ranked {
		ch := chunks[i]
		key := chunkKey(ch)
		if _, ok := byKey[key]; !ok {
			byKey[key] = codeindex.Hit{Path: ch.Path, Symbol: ch.Symbol, Start: ch.Start, End: ch.End, Text: ch.Text}
		}
		semKeys = append(semKeys, key)
	}
	var out []codeindex.Hit
	for _, key := range Fuse(k, bmKeys, semKeys) {
		out = append(out, byKey[key])
	}
	return out, ""
}

func trim(h []codeindex.Hit, k int) []codeindex.Hit {
	if len(h) > k {
		return h[:k]
	}
	return h
}

// Notes ранжирует заметки по смыслу: реализует memory.Ranker.
type Notes struct {
	E       *Engine
	Project string
}

func (n Notes) Rank(query string, notes []store.Note, k int) []store.Note {
	if n.E == nil || n.E.Model() == "" || len(notes) == 0 {
		return nil
	}
	texts := make([]string, len(notes))
	for i, nt := range notes {
		texts[i] = noteText(nt)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ranked, err := n.E.rank(ctx, n.Project, query, texts, k)
	if err != nil {
		return nil
	}
	out := make([]store.Note, len(ranked))
	for i, r := range ranked {
		out[i] = notes[r]
	}
	return out
}
