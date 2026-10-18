package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMergesGlobalAndProject(t *testing.T) {
	home, proj := t.TempDir(), t.TempDir()
	write(t, filepath.Join(home, "skills", "go-tests", "SKILL.md"), "---\nname: go-tests\ndescription: \"Общие тесты\"\n---\n\nПиши табличные тесты.\n")
	write(t, filepath.Join(home, "skills", "style.md"), "# Стиль кода\nКороткие функции.\n")
	write(t, filepath.Join(proj, ProjectDir, "go-tests", "SKILL.md"), "---\nname: go-tests\ndescription: Тесты этого проекта\n---\nИспользуй testify.\n")
	write(t, filepath.Join(proj, ProjectDir, "broken", "SKILL.md"), "---\nname: ../evil\n---\nx")
	write(t, filepath.Join(proj, ProjectDir, "notes.txt"), "не скилл")

	list, err := Load(Dirs(home, proj))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("скиллы: %+v", list)
	}
	g, _ := Find(list, "go-tests")
	if g.Scope != Project || g.Description != "Тесты этого проекта" || g.Body != "Используй testify." {
		t.Fatalf("скилл проекта должен перекрыть общий: %+v", g)
	}
	s, _ := Find(list, "style")
	if s.Scope != Global || s.Description != "Стиль кода" {
		t.Fatalf("описание из первой строки: %+v", s)
	}
	if c := Catalog(list); !strings.Contains(c, "go-tests — Тесты этого проекта") || !strings.Contains(c, "use_skill") {
		t.Fatalf("каталог: %s", c)
	}
}

func TestSaveAndRemove(t *testing.T) {
	dir := t.TempDir()
	if _, err := Save(dir, Skill{Name: "../x", Body: "a"}); err == nil {
		t.Fatal("имя с ../ должно отклоняться")
	}
	if _, err := Save(dir, Skill{Name: "ok", Body: "  "}); err == nil {
		t.Fatal("пустой скилл должен отклоняться")
	}
	path, err := Save(dir, Skill{Name: "миграции", Description: "Как писать\nмиграции", Body: "Шаги…"})
	if err != nil {
		t.Fatal(err)
	}
	list, _ := Load([]Dir{{Path: dir, Scope: Global}})
	if len(list) != 1 || list[0].Name != "миграции" || list[0].Description != "Как писать миграции" || list[0].Path != path {
		t.Fatalf("после сохранения: %+v", list)
	}
	if err := Remove(list[0]); err != nil {
		t.Fatal(err)
	}
	if list, _ := Load([]Dir{{Path: dir}}); len(list) != 0 {
		t.Fatalf("скилл не удалён: %+v", list)
	}
}
