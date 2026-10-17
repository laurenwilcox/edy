//go:build !desktop

// Без тега desktop окно не собирается: Wails нужен CGO и системный WebKit.
// Обычные `go build ./...` и `go test ./...` при этом работают.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "Окно Edy собирается так: scripts/build-macos-app.sh (на Mac, с Go и Xcode Command Line Tools).")
	os.Exit(1)
}
