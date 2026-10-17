// Package codeindex — Context Engine Edy: «скелет» проекта (типы и функции
// без тел) и поиск нужного куска кода, чтобы агенту не приходилось читать
// файлы целиком. Без CGO: Go разбирается встроенным go/ast, Python — модулем
// ast самого Python (python3), остальные файлы режутся на куски по строкам.
package codeindex

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Symbol — объявление в коде: функция, метод, тип, класс.
type Symbol struct {
	Kind      string `json:"kind"` // func, method, type, def, class, chunk
	Name      string `json:"name"`
	Signature string `json:"sig"`
	Line      int    `json:"line"`
	End       int    `json:"end"`
}

// File — разобранный файл проекта.
type File struct {
	Path    string // от корня проекта, через «/»
	Lang    string // go, python, text
	Lines   int
	Symbols []Symbol
	Note    string // ошибка разбора, если была
}

// Index — разобранный проект.
type Index struct {
	Root  string
	Files []File
}

var skipDirs = map[string]bool{".git": true, ".edy": true, "node_modules": true, "vendor": true, "__pycache__": true, ".venv": true, "venv": true, "dist": true, "build": true, ".idea": true, ".vscode": true}

// Текстовые файлы, которые имеет смысл искать, кроме .go и .py.
var textExt = map[string]bool{".md": true, ".txt": true, ".js": true, ".ts": true, ".tsx": true, ".jsx": true, ".html": true, ".css": true,
	".json": true, ".yaml": true, ".yml": true, ".toml": true, ".sql": true, ".sh": true, ".mod": true, ".cfg": true, ".ini": true, ".proto": true}

const (
	maxFiles     = 3000
	maxFileBytes = 512 << 10
	chunkLines   = 60
)

// Build разбирает проект.
func Build(ctx context.Context, root string) (*Index, error) {
	idx := &Index{Root: root}
	var pyFiles []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if len(idx.Files) >= maxFiles {
			return fs.SkipAll
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxFileBytes || !info.Mode().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		ext := strings.ToLower(filepath.Ext(p))
		switch {
		case ext == ".go":
			idx.Files = append(idx.Files, parseGo(p, rel))
		case ext == ".py":
			pyFiles = append(pyFiles, rel)
		case textExt[ext] || d.Name() == "Makefile" || d.Name() == "Dockerfile":
			idx.Files = append(idx.Files, File{Path: rel, Lang: "text", Lines: countLines(p)})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(pyFiles) > 0 {
		idx.Files = append(idx.Files, parsePython(ctx, root, pyFiles)...)
	}
	sort.Slice(idx.Files, func(i, j int) bool { return idx.Files[i].Path < idx.Files[j].Path })
	return idx, nil
}

func countLines(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		n++
	}
	return n
}

// ---- Go ----

func parseGo(path, rel string) File {
	f := File{Path: rel, Lang: "go", Lines: countLines(path)}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		f.Note = "не разобран: " + firstLine(err.Error())
		return f
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			kind := "func"
			recv := ""
			if d.Recv != nil && len(d.Recv.List) > 0 {
				kind = "method"
				recv = "(" + nodeString(fset, d.Recv.List[0].Type) + ") "
			}
			sig := "func " + recv + d.Name.Name + strings.TrimPrefix(nodeString(fset, d.Type), "func")
			name := d.Name.Name
			if recv != "" {
				name = strings.Trim(strings.TrimPrefix(recv, "(*"), "() ") + "." + d.Name.Name
				name = strings.TrimPrefix(name, "*")
			}
			f.Symbols = append(f.Symbols, Symbol{Kind: kind, Name: name, Signature: sig, Line: fset.Position(d.Pos()).Line, End: fset.Position(d.End()).Line})
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				continue
			}
			for _, spec := range d.Specs {
				ts := spec.(*ast.TypeSpec)
				sig := "type " + ts.Name.Name + " " + typeSummary(fset, ts.Type)
				f.Symbols = append(f.Symbols, Symbol{Kind: "type", Name: ts.Name.Name, Signature: sig, Line: fset.Position(ts.Pos()).Line, End: fset.Position(ts.End()).Line})
			}
		}
	}
	return f
}

// typeSummary описывает тип коротко: у структуры — имена полей, у интерфейса — методы.
func typeSummary(fset *token.FileSet, t ast.Expr) string {
	switch t := t.(type) {
	case *ast.StructType:
		var names []string
		for _, fl := range t.Fields.List {
			if len(fl.Names) == 0 {
				names = append(names, nodeString(fset, fl.Type))
			}
			for _, n := range fl.Names {
				names = append(names, n.Name)
			}
		}
		return "struct { " + strings.Join(names, ", ") + " }"
	case *ast.InterfaceType:
		var names []string
		for _, m := range t.Methods.List {
			for _, n := range m.Names {
				names = append(names, n.Name)
			}
		}
		return "interface { " + strings.Join(names, ", ") + " }"
	default:
		s := nodeString(fset, t)
		if len(s) > 80 {
			s = s[:80] + "…"
		}
		return s
	}
}

func nodeString(fset *token.FileSet, n ast.Node) string {
	var b bytes.Buffer
	printer.Fprint(&b, fset, n)
	return strings.Join(strings.Fields(b.String()), " ")
}

// ---- Python ----

//go:embed pyskeleton.py
var pySkeleton string

var pyDefRe = regexp.MustCompile(`^(\s*)(async\s+def|def|class)\s+(\w+)`)

// parsePython разбирает файлы модулем ast из python3. Если Python не
// установлен или упал, объявления ищутся по строкам «def»/«class».
func parsePython(ctx context.Context, root string, rels []string) []File {
	out := make([]File, 0, len(rels))
	parsed := map[string]struct {
		Items []Symbol `json:"items"`
		Error string   `json:"error"`
	}{}
	if py, err := exec.LookPath("python3"); err == nil {
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		cmd := exec.CommandContext(cctx, py, "-I", "-c", pySkeleton)
		cmd.Dir = root
		cmd.Stdin = strings.NewReader(strings.Join(rels, "\n"))
		data, err := cmd.Output()
		cancel()
		if err == nil {
			json.Unmarshal(data, &parsed)
		}
	}
	for _, rel := range rels {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		f := File{Path: rel, Lang: "python", Lines: countLines(abs)}
		if p, ok := parsed[rel]; ok {
			f.Symbols = p.Items
			if p.Error != "" {
				f.Note = "не разобран: " + firstLine(p.Error)
			}
		} else {
			f.Symbols = regexPython(abs)
		}
		out = append(out, f)
	}
	return out
}

func regexPython(path string) []Symbol {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var syms []Symbol
	for i, line := range strings.Split(string(data), "\n") {
		if m := pyDefRe.FindStringSubmatch(line); m != nil {
			kind := "def"
			if m[2] == "class" {
				kind = "class"
			}
			syms = append(syms, Symbol{Kind: kind, Name: m[3], Signature: strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ":")), Line: i + 1, End: i + 1})
		}
	}
	return syms
}

// ---- Скелет ----

// Skeleton — описание проекта для модели: файлы и объявления без тел.
// limit — максимальный размер в символах.
func (idx *Index) Skeleton(limit int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Файлов: %d\n", len(idx.Files))
	for _, f := range idx.Files {
		if b.Len() > limit {
			fmt.Fprintf(&b, "…(скелет обрезан, используйте search_code)\n")
			break
		}
		fmt.Fprintf(&b, "\n%s (%d строк)", f.Path, f.Lines)
		if f.Note != "" {
			fmt.Fprintf(&b, " — %s", f.Note)
		}
		b.WriteByte('\n')
		for _, s := range f.Symbols {
			indent := "  "
			if strings.Contains(s.Name, ".") && f.Lang == "python" {
				indent = "    "
			}
			fmt.Fprintf(&b, "%s%d: %s\n", indent, s.Line, s.Signature)
		}
	}
	return b.String()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
