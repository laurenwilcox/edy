// Package skills — скиллы: готовые инструкции для агентов («как у нас
// пишут тесты», «как оформлять миграции»). Формат совместим со скиллами
// Claude Code: папка <имя>/SKILL.md с заголовком
//
//	---
//	name: go-tests
//	description: Как писать тесты в этом проекте
//	---
//	текст инструкции…
//
// Допускается и одиночный файл <имя>.md. Скиллы лежат в общей папке Edy
// (skills/) и в проекте (.edy/skills/); скилл проекта важнее общего.
package skills

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Skill — одна инструкция.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body,omitempty"`
	Path        string `json:"path"`
	Scope       string `json:"scope"` // global или project
}

// Scope.
const (
	Global  = "global"
	Project = "project"
)

// ProjectDir — папка скиллов внутри проекта.
const ProjectDir = ".edy/skills"

// Dirs возвращает папки скиллов в порядке возрастания приоритета.
func Dirs(home, project string) []Dir {
	var d []Dir
	if home != "" {
		d = append(d, Dir{Path: filepath.Join(home, "skills"), Scope: Global})
	}
	if project != "" {
		d = append(d, Dir{Path: filepath.Join(project, ProjectDir), Scope: Project})
	}
	return d
}

// Dir — папка со скиллами.
type Dir struct {
	Path  string
	Scope string
}

const maxBody = 64 << 10

var validName = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N}_.-]{0,63}$`)

// ValidName сообщает, годится ли имя скилла (оно же имя папки).
func ValidName(name string) bool { return validName.MatchString(name) }

// Load читает скиллы из папок; при совпадении имён побеждает более поздняя
// папка. Битые файлы пропускаются: один неудачный скилл не ломает остальные.
func Load(dirs []Dir) ([]Skill, error) {
	byName := map[string]Skill{}
	for _, d := range dirs {
		entries, err := os.ReadDir(d.Path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		for _, e := range entries {
			var path, fallback string
			switch {
			case e.IsDir():
				path, fallback = filepath.Join(d.Path, e.Name(), "SKILL.md"), e.Name()
			case strings.HasSuffix(strings.ToLower(e.Name()), ".md"):
				path, fallback = filepath.Join(d.Path, e.Name()), strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
			default:
				continue
			}
			s, err := parse(path, fallback)
			if err != nil {
				continue
			}
			s.Scope = d.Scope
			byName[s.Name] = s
		}
	}
	out := make([]Skill, 0, len(byName))
	for _, s := range byName {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Find ищет скилл по имени.
func Find(list []Skill, name string) (Skill, bool) {
	for _, s := range list {
		if s.Name == name {
			return s, true
		}
	}
	return Skill{}, false
}

func parse(path, fallback string) (Skill, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, err
	}
	if len(data) > maxBody {
		return Skill{}, fmt.Errorf("%s: больше %d КБ", path, maxBody>>10)
	}
	meta, body := splitFrontMatter(string(data))
	s := Skill{Name: meta["name"], Description: meta["description"], Body: strings.TrimSpace(body), Path: path}
	if s.Name == "" {
		s.Name = fallback
	}
	if !ValidName(s.Name) {
		return Skill{}, fmt.Errorf("%s: недопустимое имя %q", path, s.Name)
	}
	if s.Description == "" {
		s.Description = firstLine(s.Body)
	}
	return s, nil
}

// splitFrontMatter разбирает простой YAML-заголовок «ключ: значение».
func splitFrontMatter(text string) (map[string]string, string) {
	meta := map[string]string{}
	text = strings.TrimPrefix(text, "\uFEFF")
	if !strings.HasPrefix(text, "---") {
		return meta, text
	}
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64<<10), maxBody)
	sc.Scan() // ---
	consumed := len(sc.Text()) + 1
	for sc.Scan() {
		line := sc.Text()
		consumed += len(line) + 1
		if strings.TrimSpace(line) == "---" {
			if consumed > len(text) {
				consumed = len(text)
			}
			return meta, text[consumed:]
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		meta[strings.TrimSpace(k)] = v
	}
	// Заголовок не закрыт — считаем весь файл текстом.
	return map[string]string{}, text
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	line = strings.TrimLeft(line, "# ")
	if r := []rune(line); len(r) > 120 {
		line = string(r[:120]) + "…"
	}
	return line
}

// Save создаёт или перезаписывает скилл в папке dir как <имя>/SKILL.md.
func Save(dir string, s Skill) (string, error) {
	if !ValidName(s.Name) {
		return "", fmt.Errorf("имя %q: только буквы, цифры, «-», «_» и «.», до 64 символов", s.Name)
	}
	if strings.TrimSpace(s.Body) == "" {
		return "", errors.New("пустой текст скилла")
	}
	if len(s.Body) > maxBody {
		return "", fmt.Errorf("скилл больше %d КБ", maxBody>>10)
	}
	desc := strings.ReplaceAll(strings.TrimSpace(s.Description), "\n", " ")
	path := filepath.Join(dir, s.Name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	content := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n%s\n", s.Name, desc, strings.TrimSpace(s.Body))
	return path, os.WriteFile(path, []byte(content), 0o644)
}

// Remove удаляет найденный Load скилл: папку со SKILL.md или одиночный файл.
func Remove(s Skill) error {
	if filepath.Base(s.Path) == "SKILL.md" {
		return os.RemoveAll(filepath.Dir(s.Path))
	}
	return os.Remove(s.Path)
}

// Catalog — короткий список скиллов для системного промпта агента.
func Catalog(list []Skill) string {
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Доступные скиллы (инструкции этого проекта). Если скилл подходит к задаче, сначала загрузи его через use_skill:\n")
	for _, s := range list {
		fmt.Fprintf(&b, "- %s — %s\n", s.Name, s.Description)
	}
	return b.String()
}
