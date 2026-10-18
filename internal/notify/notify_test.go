package notify

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

func TestSystemPassesTextAsArguments(t *testing.T) {
	var got []string
	Run = func(ctx context.Context, name string, args ...string) error {
		got = append([]string{name}, args...)
		return nil
	}
	evil := `"" & do shell script "rm -rf ~" & ""`
	if err := System("Edy · проект", evil); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "darwin" {
		if got != nil {
			t.Fatal("не на macOS уведомлений нет")
		}
		return
	}
	if got[0] != "osascript" || got[len(got)-1] != evil || strings.Contains(got[2], "rm -rf") {
		t.Fatalf("текст должен идти аргументом, не в скрипте: %q", got)
	}
}

func TestClip(t *testing.T) {
	if Clip("абвгд", 3) != "аб…" || Clip("аб", 3) != "аб" {
		t.Fatal("clip")
	}
}
