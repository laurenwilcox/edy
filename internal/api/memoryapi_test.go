package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/laurenwilcox/name/internal/store"
)

func TestMemoryAPIAndOrchestratorBrief(t *testing.T) {
	h, st := setup(t)
	st.AddProvider(store.Provider{Name: "agy", Kind: store.KindCLI, Command: []string{"agy", "{prompt}"}, Models: []string{"m"}})
	p := &planner{}
	h.LLM = p
	dir := gitProject(t)
	call(h, "POST", "/api/projects", fmt.Sprintf(`{"path":%q}`, dir))

	for _, n := range []string{
		`{"text":"Тесты запускаются: go test ./...","kind":"fact","pinned":true}`,
		`{"text":"Модуль оплаты хрупкий — возвраты без тестов не трогать","kind":"lesson"}`,
		`{"text":"Логи в /var/log/app"}`,
	} {
		var body map[string]any
		json.Unmarshal([]byte(n), &body)
		body["project"] = dir
		b, _ := json.Marshal(body)
		if rec := call(h, "POST", "/api/memory", string(b)); rec.Code != http.StatusCreated {
			t.Fatalf("добавить: %d %s", rec.Code, rec.Body)
		}
	}
	if rec := call(h, "POST", "/api/memory", fmt.Sprintf(`{"project":%q,"text":%q}`, dir, strings.Repeat("я", 401))); rec.Code != http.StatusBadRequest {
		t.Fatalf("длинная заметка: %d", rec.Code)
	}
	var res struct {
		Notes []store.Note `json:"notes"`
	}
	json.Unmarshal(call(h, "GET", "/api/memory?project="+dir+"&q=оплата", "").Body.Bytes(), &res)
	if len(res.Notes) != 1 || res.Notes[0].Kind != store.NoteLesson {
		t.Fatalf("поиск: %+v", res.Notes)
	}

	// Оркестратор видит закреплённое и похожее на просьбу, но не всё подряд.
	call(h, "POST", "/api/orchestrator", fmt.Sprintf(`{"project":%q,"model":"agy/m","messages":[{"role":"user","content":"почини возврат в оплате"}]}`, dir))
	if !strings.Contains(p.system, "Память проекта") || !strings.Contains(p.system, "go test ./...") || !strings.Contains(p.system, "Модуль оплаты хрупкий") || strings.Contains(p.system, "/var/log/app") {
		t.Fatalf("память в инструкциях Оркестратора:\n%s", p.system)
	}

	json.Unmarshal(call(h, "GET", "/api/memory?project="+dir, "").Body.Bytes(), &res)
	id := res.Notes[0].ID
	if rec := call(h, "PUT", fmt.Sprintf("/api/memory/%d", id), fmt.Sprintf(`{"project":%q,"pinned":true}`, dir)); rec.Code != http.StatusOK {
		t.Fatalf("закрепить: %d", rec.Code)
	}
	if rec := call(h, "DELETE", fmt.Sprintf("/api/memory/%d?project=/чужой", id), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("чужой проект не удаляет: %d", rec.Code)
	}
	if rec := call(h, "DELETE", fmt.Sprintf("/api/memory/%d?project=%s", id, dir), ""); rec.Code != http.StatusOK {
		t.Fatalf("удалить: %d", rec.Code)
	}
}
