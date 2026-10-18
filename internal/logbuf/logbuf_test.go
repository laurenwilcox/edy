package logbuf

import (
	"fmt"
	"testing"
)

func TestBuffer(t *testing.T) {
	b := New(nil)
	fmt.Fprint(b, "один\nдва\nтр")
	fmt.Fprint(b, "и\n")
	lines, seq := b.Lines(0)
	if seq != 3 || len(lines) != 3 || lines[2] != "три" {
		t.Fatalf("строки: %q, %d", lines, seq)
	}
	if lines, _ := b.Lines(2); len(lines) != 1 || lines[0] != "три" {
		t.Fatalf("после 2: %q", lines)
	}
	for i := 0; i < Size+5; i++ {
		fmt.Fprintf(b, "s%d\n", i)
	}
	lines, seq = b.Lines(0)
	if len(lines) != Size || lines[Size-1] != fmt.Sprintf("s%d", Size+4) || seq != Size+8 {
		t.Fatalf("переполнение: %d строк, последняя %q, seq %d", len(lines), lines[len(lines)-1], seq)
	}
	if lines, _ := b.Lines(seq - 2); len(lines) != 2 {
		t.Fatalf("хвост: %q", lines)
	}
}
