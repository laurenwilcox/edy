package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/laurenwilcox/name/internal/store"
	"github.com/laurenwilcox/name/internal/tasks"
)

func TestGitTabAndAgentJournal(t *testing.T) {
	h, st := setup(t)
	st.AddProvider(store.Provider{Name: "agy", Kind: store.KindCLI, Command: []string{"agy", "{prompt}"}, Models: []string{"m"}})
	b := &brain{}
	h.LLM = b
	h.Tasks = tasks.NewManager(b)
	h.Tasks.Journal = st
	h.Tasks.Finalize = Finalizer(st)
	h.ReposDir = filepath.Join(t.TempDir(), "repos")

	// «Удалённый» репозиторий: bare-копия тестового проекта.
	src := gitProject(t)
	owner := filepath.Join(t.TempDir(), "vova")
	os.MkdirAll(owner, 0o755)
	bare := filepath.Join(owner, "calc.git")
	if out, err := exec.Command("git", "clone", "-q", "--bare", src, bare).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}

	// Аккаунты.
	if rec := call(h, "POST", "/api/git/accounts", `{"kind":"gitea","token":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("Gitea без адреса: %d", rec.Code)
	}
	rec := call(h, "POST", "/api/git/accounts", `{"kind":"github","label":"личный","token":"ghp_test"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("account: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "GET", "/api/git/accounts", ""); strings.Contains(rec.Body.String(), "ghp_test") || !strings.Contains(rec.Body.String(), `"host":"https://github.com"`) {
		t.Fatalf("список аккаунтов: %s", rec.Body)
	}

	// Клон → проект.
	rec = call(h, "POST", "/api/git/clone", fmt.Sprintf(`{"url":"file://%s"}`, bare))
	var p projectJSON
	json.Unmarshal(rec.Body.Bytes(), &p)
	if rec.Code != http.StatusOK || !p.Git || !p.Ready || p.Name != "calc" {
		t.Fatalf("clone: %d %s", rec.Code, rec.Body)
	}
	clone := p.Path

	// Настройки: коммит прямо в main и push.
	if rec := call(h, "PUT", "/api/git/settings", fmt.Sprintf(`{"path":%q,"settings":{"commit":"direct","push":"pr"}}`, clone)); rec.Code != http.StatusBadRequest {
		t.Fatalf("PR без новой ветки: %d", rec.Code)
	}
	if rec := call(h, "PUT", "/api/git/settings", fmt.Sprintf(`{"path":%q,"settings":{"commit":"direct","push":"push","author_name":"Edy Bot"}}`, clone)); rec.Code != http.StatusBadRequest {
		t.Fatalf("автор без email: %d", rec.Code)
	}
	if rec := call(h, "PUT", "/api/git/settings", fmt.Sprintf(`{"path":%q,"settings":{"commit":"direct","push":"push","prefix":"[edy] ","author_name":"Edy Bot","author_email":"bot@edy"}}`, clone)); rec.Code != http.StatusNoContent {
		t.Fatalf("settings: %d %s", rec.Code, rec.Body)
	}

	// Оркестратор → задача → Approve по настройкам.
	call(h, "POST", "/api/orchestrator", fmt.Sprintf(`{"project":%q,"model":"agy/m","conversation":"c1","messages":[{"role":"user","content":"почини сложение"}]}`, clone))
	rec = call(h, "POST", "/api/tasks", fmt.Sprintf(`{"project":%q,"request":"почини сложение","plan":"1. a+b","models":["agy/m"],"conversation":"c1"}`, clone))
	var v tasks.View
	json.Unmarshal(rec.Body.Bytes(), &v)
	for i := 0; i < 600 && v.State == tasks.StateRunning; i++ {
		time.Sleep(50 * time.Millisecond)
		json.Unmarshal(call(h, "GET", "/api/tasks/"+v.ID, "").Body.Bytes(), &v)
	}
	if v.State != tasks.StateDone {
		t.Fatalf("state = %s %s", v.State, v.Error)
	}
	json.Unmarshal(call(h, "POST", "/api/tasks/"+v.ID+"/approve", "").Body.Bytes(), &v)
	if v.State != tasks.StateApproved || !strings.Contains(v.Note, "отправлено в origin") {
		t.Fatalf("approve: %+v", v)
	}
	if got, _ := exec.Command("git", "-C", bare, "log", "-1", "--format=%an|%s", "main").Output(); strings.TrimSpace(string(got)) != "Edy Bot|[edy] почини сложение" {
		t.Fatalf("в origin/main: %q", got)
	}

	// Вкладка «Git»: состояние.
	var info gitInfoJSON
	json.Unmarshal(call(h, "GET", "/api/git/info?path="+clone, "").Body.Bytes(), &info)
	if !info.IsRepo || info.Branch != "main" || info.Settings.Commit != "direct" || !strings.Contains(info.Log[0], "[edy] почини сложение") {
		t.Fatalf("info = %+v", info)
	}
	if rec := call(h, "POST", "/api/git/pull", fmt.Sprintf(`{"path":%q}`, clone)); rec.Code != http.StatusOK {
		t.Fatalf("pull: %d %s", rec.Code, rec.Body)
	}

	// Журнал агентов: Оркестратор и исполнитель.
	var agents []store.Agent
	json.Unmarshal(call(h, "GET", "/api/agents", "").Body.Bytes(), &agents)
	roles := map[string]store.Agent{}
	for _, a := range agents {
		roles[a.Role] = a
	}
	orch, exec_ := roles[store.RoleOrchestrator], roles[store.RoleExecutor]
	if orch.ID != "orch-c1" || orch.State != store.AgentDone || exec_.State != store.AgentDone || exec_.TaskID != v.ID || exec_.Events == 0 {
		t.Fatalf("агенты = %+v", agents)
	}
	var detail struct {
		Agent  store.Agent        `json:"agent"`
		Events []store.AgentEvent `json:"events"`
	}
	json.Unmarshal(call(h, "GET", "/api/agents/orch-c1", "").Body.Bytes(), &detail)
	var kinds []string
	for _, e := range detail.Events {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "user,model,plan,info" {
		t.Fatalf("события Оркестратора: %v", kinds)
	}

	// Обычная папка без git тоже проект.
	plain := t.TempDir()
	json.Unmarshal(call(h, "POST", "/api/projects", fmt.Sprintf(`{"path":%q}`, plain)).Body.Bytes(), &p)
	if p.Git || !p.Ready {
		t.Fatalf("папка без git: %+v", p)
	}
}
