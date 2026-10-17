package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laurenwilcox/name/internal/omniroute"
)

func TestEnsureInstalled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	pr := omniroute.CLIPreset{Command: []string{"edy-fake-tool", "{prompt}"}, Install: "install edy-fake-tool"}

	var ran []string
	install := func(cmd string) error {
		ran = append(ran, cmd)
		return os.WriteFile(filepath.Join(dir, "edy-fake-tool"), []byte("#!/bin/sh\n"), 0o755)
	}

	// Отказ: ничего не запускаем.
	var out bytes.Buffer
	if _, err := ensureInstalled(pr, false, strings.NewReader("n\n"), &out, install); err == nil || len(ran) != 0 {
		t.Fatalf("err = %v, ran = %v", err, ran)
	}
	if !strings.Contains(out.String(), "install edy-fake-tool") {
		t.Fatalf("команда установки не показана: %s", out.String())
	}

	// Согласие: ставим и возвращаем полный путь.
	bin, err := ensureInstalled(pr, false, strings.NewReader("да\n"), &out, install)
	if err != nil || bin != filepath.Join(dir, "edy-fake-tool") || len(ran) != 1 {
		t.Fatalf("bin = %q, err = %v, ran = %v", bin, err, ran)
	}

	// Уже установлена: установщик не вызывается.
	if _, err := ensureInstalled(pr, false, strings.NewReader(""), &out, install); err != nil || len(ran) != 1 {
		t.Fatalf("err = %v, ran = %v", err, ran)
	}
}
