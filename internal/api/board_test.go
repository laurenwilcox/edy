package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/laurenwilcox/name/internal/store"
	"github.com/laurenwilcox/name/internal/tasks"
)

func TestBoardAPI(t *testing.T) {
	h, st := setup(t)
	st.AddProvider(store.Provider{Name: "agy", Kind: store.KindCLI, Command: []string{"agy", "{prompt}"}, Models: []string{"m"}})
	b := &brain{}
	h.LLM = b
	h.Tasks = tasks.NewManager(b, st)
	dir := gitProject(t)
	call(h, "POST", "/api/projects", fmt.Sprintf(`{"path":%q}`, dir))

	var idea tasks.View
	rec := call(h, "POST", "/api/board", fmt.Sprintf(`{"project":%q,"request":"Починить сложение\nсрочно"}`, dir))
	json.Unmarshal(rec.Body.Bytes(), &idea)
	if rec.Code != http.StatusCreated || idea.State != tasks.StateBacklog || idea.Title != "Починить сложение" {
		t.Fatalf("пул: %d %s", rec.Code, rec.Body)
	}
	var sub tasks.View
	json.Unmarshal(call(h, "POST", "/api/board", fmt.Sprintf(`{"project":%q,"parent":%q,"title":"Тест на сложение"}`, dir, idea.ID)).Body.Bytes(), &sub)
	if sub.Parent != idea.ID {
		t.Fatalf("подзадача: %+v", sub)
	}
	call(h, "PUT", "/api/tasks/"+idea.ID, `{"title":"Сложение"}`)
	call(h, "POST", "/api/tasks/"+idea.ID+"/comment", `{"text":"начать с calc.go"}`)

	// План утверждён — задача из пула уходит в работу с комментарием Оркестратора.
	body, _ := json.Marshal(map[string]any{"id": idea.ID, "project": dir, "request": "почини сложение",
		"plan": "1. calc.go: a - b → a + b", "comment": "Правка в одну строку", "models": []string{"agy/m"}})
	rec = call(h, "POST", "/api/tasks", string(body))
	var v tasks.View
	json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Code != http.StatusCreated || v.ID != idea.ID {
		t.Fatalf("в работу: %d %s", rec.Code, rec.Body)
	}
	deadline := time.Now().Add(time.Minute)
	for (v.State == tasks.StateRunning || v.State == tasks.StateQueued) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		json.Unmarshal(call(h, "GET", "/api/tasks/"+v.ID, "").Body.Bytes(), &v)
	}
	if v.State != tasks.StateDone || v.Title != "Сложение" {
		t.Fatalf("итог: %s %s", v.State, v.Title)
	}
	var history []string
	for _, e := range v.Events {
		history = append(history, e.Kind+": "+e.Text)
	}
	joined := strings.Join(history, "\n")
	for _, want := range []string{"created: Задача добавлена в пул", "subtask: Добавлена подзадача: Тест на сложение",
		"comment: Вы: начать с calc.go", "plan: 1. calc.go", "comment: Оркестратор: Правка в одну строку", "queued: ", "hooks: "} {
		if !strings.Contains(joined, want) {
			t.Fatalf("в истории нет %q:\n%s", want, joined)
		}
	}
	if rec := call(h, "DELETE", "/api/tasks/"+v.ID, ""); rec.Code != http.StatusConflict {
		t.Fatalf("задачу, ждущую решения, удалить нельзя: %d", rec.Code)
	}
	var list []tasks.View
	json.Unmarshal(call(h, "GET", "/api/tasks?project="+dir, "").Body.Bytes(), &list)
	if len(list) != 2 {
		t.Fatalf("доска проекта: %d", len(list))
	}
	if rec := call(h, "DELETE", "/api/tasks/"+sub.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("удаление из пула: %d", rec.Code)
	}
	h.Tasks.Wait()
}

func TestPatchRevertAndFileViewer(t *testing.T) {
	h, st := setup(t)
	st.AddProvider(store.Provider{Name: "agy", Kind: store.KindCLI, Command: []string{"agy", "{prompt}"}, Models: []string{"m"}})
	b := &brain{}
	h.LLM = b
	h.Tasks = tasks.NewManager(b, st)
	dir := gitProject(t)
	call(h, "POST", "/api/projects", fmt.Sprintf(`{"path":%q}`, dir))

	var files struct{ Files []string }
	json.Unmarshal(call(h, "GET", "/api/projects/files?path="+dir, "").Body.Bytes(), &files)
	if strings.Join(files.Files, ",") != "calc.go,calc_test.go,go.mod" {
		t.Fatalf("файлы: %v", files.Files)
	}
	var f struct{ Text string }
	json.Unmarshal(call(h, "GET", "/api/projects/file?path="+dir+"&file=calc.go", "").Body.Bytes(), &f)
	if !strings.Contains(f.Text, "func Add") {
		t.Fatalf("файл: %q", f.Text)
	}
	for _, bad := range []string{"../etc/passwd", ".git/config", "/etc/passwd"} {
		if rec := call(h, "GET", "/api/projects/file?path="+dir+"&file="+bad, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", bad, rec.Code)
		}
	}

	var v tasks.View
	json.Unmarshal(call(h, "POST", "/api/tasks", fmt.Sprintf(`{"project":%q,"request":"почини","models":["agy/m"]}`, dir)).Body.Bytes(), &v)
	deadline := time.Now().Add(time.Minute)
	for (v.State == tasks.StateRunning || v.State == tasks.StateQueued) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		json.Unmarshal(call(h, "GET", "/api/tasks/"+v.ID, "").Body.Bytes(), &v)
	}
	var p struct{ Patch string }
	json.Unmarshal(call(h, "GET", "/api/tasks/"+v.ID+"/patch", "").Body.Bytes(), &p)
	if !strings.Contains(p.Patch, "+func Add(a, b int) int { return a + b }") {
		t.Fatalf("patch:\n%s", p.Patch)
	}
	rec := call(h, "POST", "/api/tasks/"+v.ID+"/revert-file", `{"file":"calc.go"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("откат файла: %d %s", rec.Code, rec.Body)
	}
	json.Unmarshal(call(h, "GET", "/api/tasks/"+v.ID+"/patch", "").Body.Bytes(), &p)
	json.Unmarshal(call(h, "GET", "/api/projects/file?path="+dir+"&file=calc.go", "").Body.Bytes(), &f)
	if strings.Contains(p.Patch, "calc.go") || !strings.Contains(f.Text, "a - b") {
		t.Fatalf("после отката файла: patch=%q файл=%q", p.Patch, f.Text)
	}
	if rec := call(h, "POST", "/api/tasks/"+v.ID+"/revert-file", `{"file":"../x"}`); rec.Code != http.StatusConflict {
		t.Fatalf("выход за проект: %d", rec.Code)
	}
	h.Tasks.Reject(v.ID)
	h.Tasks.Wait()
}

func TestNotifyPrefs(t *testing.T) {
	h, _ := setup(t)
	var shown []string
	h.Notify = func(title, msg string) error { shown = append(shown, title+"|"+msg); return nil }
	if rec := call(h, "GET", "/api/prefs", ""); !strings.Contains(rec.Body.String(), `"notify":true`) {
		t.Fatalf("по умолчанию уведомления включены: %s", rec.Body)
	}
	if rec := call(h, "PUT", "/api/prefs", `{"notify":false}`); !strings.Contains(rec.Body.String(), `"notify":false`) {
		t.Fatalf("выключить: %s", rec.Body)
	}
	if off, _ := h.Store.Setting(SettingNotifyOff); off != "1" {
		t.Fatal("настройка не сохранена")
	}
	call(h, "POST", "/api/notify/test", "")
	if len(shown) != 1 || !strings.HasPrefix(shown[0], "Edy|") {
		t.Fatalf("пробное уведомление: %v", shown)
	}
}
