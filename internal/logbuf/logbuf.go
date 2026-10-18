// Package logbuf — журнал работы Edy в памяти: последние строки логов для
// экрана «Логи». Каждая запись дублируется в следующий Writer (обычно stderr).
package logbuf

import (
	"bytes"
	"io"
	"strings"
	"sync"
)

// Size — сколько последних строк хранится.
const Size = 2000

// Buffer — потокобезопасный кольцевой буфер строк.
type Buffer struct {
	mu    sync.Mutex
	next  io.Writer
	lines []string
	start int    // индекс самой старой строки, когда буфер заполнен
	part  []byte // хвост без перевода строки
	seq   int    // сколько строк записано за всё время
}

// New создаёт буфер, который дублирует записи в next (nil — никуда).
func New(next io.Writer) *Buffer {
	return &Buffer{next: next}
}

// Write режет данные на строки и запоминает их.
func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.part = append(b.part, p...)
	for {
		i := bytes.IndexByte(b.part, '\n')
		if i < 0 {
			break
		}
		b.add(strings.TrimRight(string(b.part[:i]), "\r"))
		b.part = b.part[i+1:]
	}
	b.mu.Unlock()
	if b.next != nil {
		b.next.Write(p)
	}
	return len(p), nil
}

func (b *Buffer) add(line string) {
	b.seq++
	if len(b.lines) < Size {
		b.lines = append(b.lines, line)
		return
	}
	b.lines[b.start] = line
	b.start = (b.start + 1) % Size
}

// Lines возвращает строки, записанные после строки с номером since
// (0 — все сохранённые), и номер последней строки.
func (b *Buffer) Lines(since int) ([]string, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(b.lines)
	skip := 0
	if since > 0 {
		first := b.seq - n // номер строки перед самой старой
		skip = since - first
		if skip < 0 {
			skip = 0
		}
		if skip > n {
			skip = n
		}
	}
	out := make([]string, 0, n-skip)
	for i := skip; i < n; i++ {
		out = append(out, b.lines[(b.start+i)%n])
	}
	return out, b.seq
}

// Clear очищает журнал.
func (b *Buffer) Clear() {
	b.mu.Lock()
	b.lines, b.start = nil, 0
	b.mu.Unlock()
}
