package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestIssueImportClosesOnApprove(t *testing.T) {
	h, st := setup(t)
	st.AddProvider(store.Provider{Name: "agy", Kind: store.KindCLI, Command: []string{"agy", "{prompt}"}, Models: []string{"m"}})
	b := &brain{}
	h.LLM = b
	h.Tasks = tasks.NewManager(b, st)
	var summary string
	h.Tasks.Finalize = func(ctx context.Context, project, goal, s, model string) (tasks.Outcome, error) {
		summary = s
		return tasks.Outcome{Note: "ok"}, nil
	}
	gitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/vova/calc/issues/3" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"number":3,"title":"Сложение вычитает","body":"Add(2,3) возвращает -1","html_url":"http://git/vova/calc/issues/3","labels":[{"name":"bug"}]}`))
	}))
	defer gitea.Close()
	dir := gitProject(t)
	call(h, "POST", "/api/projects", fmt.Sprintf(`{"path":%q}`, dir))

	rec := call(h, "POST", "/api/board/issue", fmt.Sprintf(`{"project":%q,"url":%q}`, dir, gitea.URL+"/vova/calc/issues/3"))
	var v tasks.View
	json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Code != http.StatusCreated || v.Title != "#3 Сложение вычитает" || !strings.Contains(v.Request, "Метки: bug") || v.IssueRef != "#3" {
		t.Fatalf("импорт: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/board/issue", fmt.Sprintf(`{"project":%q,"url":"https://github.com/x/y/pull/1"}`, dir)); rec.Code != http.StatusBadRequest {
		t.Fatalf("не issue: %d", rec.Code)
	}
	body, _ := json.Marshal(map[string]any{"id": v.ID, "project": dir, "request": v.Request, "models": []string{"agy/m"}})
	call(h, "POST", "/api/tasks", string(body))
	deadline := time.Now().Add(time.Minute)
	for v.State != tasks.StateDone && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		json.Unmarshal(call(h, "GET", "/api/tasks/"+v.ID, "").Body.Bytes(), &v)
	}
	call(h, "POST", "/api/tasks/"+v.ID+"/approve", "")
	if !strings.HasSuffix(summary, "Closes #3") {
		t.Fatalf("коммит должен закрывать issue: %q", summary)
	}
	h.Tasks.Wait()
}

func TestTemplates(t *testing.T) {
	h, _ := setup(t)
	call(h, "POST", "/api/templates", `{"text":"Переведи интерфейс на английский"}`)
	call(h, "POST", "/api/templates", `{"text":"Переведи интерфейс на английский"}`)
	var res struct{ Builtin, Custom []string }
	json.Unmarshal(call(h, "GET", "/api/templates", "").Body.Bytes(), &res)
	if len(res.Builtin) < 5 || len(res.Custom) != 1 {
		t.Fatalf("шаблоны: %+v", res)
	}
	json.Unmarshal(call(h, "DELETE", "/api/templates?text="+url.QueryEscape("Переведи интерфейс на английский"), "").Body.Bytes(), &res)
	if len(res.Custom) != 0 {
		t.Fatalf("удаление: %+v", res.Custom)
	}
}

func TestEpicFromOrchestratorPlan(t *testing.T) {
	h, st := setup(t)
	st.AddProvider(store.Provider{Name: "agy", Kind: store.KindCLI, Command: []string{"agy", "{prompt}"}, Models: []string{"m", "big"}})
	b := &brain{}
	h.LLM = b
	h.Tasks = tasks.NewManager(b, st)
	dir := gitProject(t)
	call(h, "POST", "/api/projects", fmt.Sprintf(`{"path":%q}`, dir))
	body, _ := json.Marshal(map[string]any{"project": dir, "request": "большая задача", "plan": "1. часть A\n2. часть B", "models": []string{"agy/big"},
		"subtasks": []map[string]any{{"title": "A", "goal": "часть A", "model": "agy/m"}, {"title": "B", "goal": "часть B", "model": "agy/big", "after": []int{1}}}})
	rec := call(h, "POST", "/api/tasks", string(body))
	var v tasks.View
	json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Code != http.StatusCreated || v.Kind != tasks.KindEpic {
		t.Fatalf("эпик: %d %s", rec.Code, rec.Body)
	}
	var list []tasks.View
	json.Unmarshal(call(h, "GET", "/api/tasks?project="+dir, "").Body.Bytes(), &list)
	subs := 0
	for _, x := range list {
		if x.Kind == tasks.KindSub && x.Parent == v.ID {
			subs++
		}
	}
	if subs != 2 {
		t.Fatalf("подзадачи на доске: %d", subs)
	}
	bad, _ := json.Marshal(map[string]any{"project": dir, "request": "x", "plan": "p", "models": []string{"agy/m"},
		"subtasks": []map[string]any{{"title": "A", "goal": "a", "model": "nope/m"}, {"title": "B", "goal": "b", "model": "agy/m"}}})
	if rec := call(h, "POST", "/api/tasks", string(bad)); rec.Code != http.StatusBadRequest {
		t.Fatalf("неизвестная модель подзадачи: %d", rec.Code)
	}
	if rec := call(h, "PUT", "/api/git/settings", fmt.Sprintf(`{"path":%q,"settings":{"commit":"branch","push":"off","max_parallel":9}}`, dir)); rec.Code != http.StatusBadRequest {
		t.Fatalf("лимит агентов: %d", rec.Code)
	}
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		if s := h.Tasks.Get(v.ID, 0).State; s != tasks.StateRunning && s != tasks.StateQueued {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.Tasks.Wait()
}
