package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	maxReadBytes = 200 << 10
	maxListed    = 500
)

// Каталоги, которые агенту не нужны или которые нельзя трогать.
var skipDirs = map[string]bool{".git": true, ".edy": true, "node_modules": true, "vendor": true, "__pycache__": true, ".venv": true, "venv": true}

// Workspace — папка проекта. Агент видит и меняет только её содержимое,
// без .git и служебной папки .edy.
type Workspace struct {
	Root string
}

// resolve превращает путь от агента в абсолютный и не даёт выйти за пределы проекта.
func (w Workspace) resolve(rel string) (string, error) {
	if rel == "" {
		rel = "."
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("путь %q должен быть относительным (от корня проекта)", rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("путь %q выходит за пределы проекта", rel)
	}
	first := strings.SplitN(clean, string(filepath.Separator), 2)[0]
	if first == ".git" || first == ".edy" {
		return "", fmt.Errorf("папка %s служебная, её трогать нельзя", first)
	}
	root, err := filepath.EvalSymlinks(w.Root)
	if err != nil {
		return "", err
	}
	abs := filepath.Join(root, clean)
	// Символические ссылки не должны уводить за пределы проекта.
	dir := abs
	for {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			if real != root && !strings.HasPrefix(real, root+string(filepath.Separator)) {
				return "", fmt.Errorf("путь %q ведёт за пределы проекта", rel)
			}
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return abs, nil
}

// Tools — описания инструментов агента.
func (w Workspace) Tools() []ToolDef {
	def := func(name, desc, params string) ToolDef {
		var t ToolDef
		t.Type = "function"
		t.Function.Name = name
		t.Function.Description = desc
		t.Function.Parameters = json.RawMessage(params)
		return t
	}
	path := `{"type":"object","properties":{"path":{"type":"string","description":"путь от корня проекта"}},"required":["path"]}`
	return []ToolDef{
		def("list_files", "Список файлов проекта (рекурсивно) в папке path.", `{"type":"object","properties":{"path":{"type":"string","description":"папка от корня проекта, по умолчанию корень"}}}`),
		def("read_file", "Прочитать файл целиком.", path),
		def("write_file", "Создать файл или полностью перезаписать его.", `{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
		def("replace_in_file", "Заменить в файле фрагмент old на new. old должен встречаться в файле ровно один раз.", `{"type":"object","properties":{"path":{"type":"string"},"old":{"type":"string"},"new":{"type":"string"}},"required":["path","old","new"]}`),
		def("delete_file", "Удалить файл.", path),
		def("finish", "Работа закончена. После этого Edy запустит линтеры и тесты.", `{"type":"object","properties":{"summary":{"type":"string","description":"что сделано, 1-3 предложения"}},"required":["summary"]}`),
	}
}

// Call выполняет инструмент. changed — менялись ли файлы (нужен снимок).
func (w Workspace) Call(name, args string) (result string, changed bool, err error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Old     string `json:"old"`
		New     string `json:"new"`
	}
	if args != "" && json.Unmarshal([]byte(args), &a) != nil {
		return "", false, fmt.Errorf("аргументы инструмента %s — не JSON", name)
	}
	switch name {
	case "list_files":
		r, err := w.list(a.Path)
		return r, false, err
	case "read_file":
		p, err := w.resolve(a.Path)
		if err != nil {
			return "", false, err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return "", false, fmt.Errorf("не удалось прочитать %s: %w", a.Path, unwrapPath(err))
		}
		if len(data) > maxReadBytes {
			return string(data[:maxReadBytes]) + "\n…(файл обрезан)", false, nil
		}
		return string(data), false, nil
	case "write_file":
		p, err := w.resolve(a.Path)
		if err != nil {
			return "", false, err
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", false, err
		}
		if err := os.WriteFile(p, []byte(a.Content), 0o644); err != nil {
			return "", false, err
		}
		return fmt.Sprintf("записано %s (%d байт)", a.Path, len(a.Content)), true, nil
	case "replace_in_file":
		p, err := w.resolve(a.Path)
		if err != nil {
			return "", false, err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return "", false, fmt.Errorf("не удалось прочитать %s: %w", a.Path, unwrapPath(err))
		}
		switch n := strings.Count(string(data), a.Old); {
		case a.Old == "":
			return "", false, errors.New("old не может быть пустым")
		case n == 0:
			return "", false, fmt.Errorf("фрагмент old не найден в %s", a.Path)
		case n > 1:
			return "", false, fmt.Errorf("фрагмент old встречается в %s %d раз — уточните его", a.Path, n)
		}
		if err := os.WriteFile(p, []byte(strings.Replace(string(data), a.Old, a.New, 1)), 0o644); err != nil {
			return "", false, err
		}
		return "заменено в " + a.Path, true, nil
	case "delete_file":
		p, err := w.resolve(a.Path)
		if err != nil {
			return "", false, err
		}
		if err := os.Remove(p); err != nil {
			return "", false, fmt.Errorf("не удалось удалить %s: %w", a.Path, unwrapPath(err))
		}
		return "удалено " + a.Path, true, nil
	}
	return "", false, fmt.Errorf("неизвестный инструмент %q", name)
}

func (w Workspace) list(rel string) (string, error) {
	start, err := w.resolve(rel)
	if err != nil {
		return "", err
	}
	root, _ := filepath.EvalSymlinks(w.Root)
	var files []string
	err = filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != start && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		r, _ := filepath.Rel(root, p)
		files = append(files, filepath.ToSlash(r))
		if len(files) >= maxListed {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	if len(files) == 0 {
		return "(файлов нет)", nil
	}
	out := strings.Join(files, "\n")
	if len(files) >= maxListed {
		out += "\n…(показаны первые " + fmt.Sprint(maxListed) + ")"
	}
	return out, nil
}

// unwrapPath убирает из ошибки абсолютный путь — агенту он не нужен.
func unwrapPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
