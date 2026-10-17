package gitops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laurenwilcox/name/internal/store"
)

func env(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("нет git")
	}
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "T", "GIT_AUTHOR_EMAIL": "t@x", "GIT_COMMITTER_NAME": "T", "GIT_COMMITTER_EMAIL": "t@x", "GIT_CONFIG_GLOBAL": os.DevNull} {
		t.Setenv(k, v)
	}
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// repoWithRemote — рабочий репозиторий, у которого origin — локальный bare-репозиторий.
func repoWithRemote(t *testing.T) (work, remote string) {
	env(t)
	remote = filepath.Join(t.TempDir(), "remote.git")
	run(t, filepath.Dir(remote), "init", "-q", "--bare", "-b", "main", remote)
	work = t.TempDir()
	run(t, work, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("1"), 0o644)
	run(t, work, "add", "-A")
	run(t, work, "commit", "-q", "-m", "init")
	run(t, work, "remote", "add", "origin", remote)
	run(t, work, "push", "-q", "-u", "origin", "main")
	return work, remote
}

func TestFinalizeModes(t *testing.T) {
	work, remote := repoWithRemote(t)
	ctx := context.Background()

	// Новая ветка + push.
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("2"), 0o644)
	ps := store.ProjectSettings{Commit: store.CommitBranch, Push: store.PushOn, Prefix: "Edy: ", AuthorName: "Edy Bot", AuthorEmail: "bot@edy"}
	res, err := Finalize(ctx, work, ps, nil, "edy/two", "Двойка", "Подробности")
	if err != nil || !res.Committed || !res.Pushed || res.Branch != "edy/two" {
		t.Fatalf("branch+push: %+v %v", res, err)
	}
	if got := run(t, remote, "log", "-1", "--format=%an|%s", "edy/two"); got != "Edy Bot|Edy: Двойка" {
		t.Fatalf("в origin: %q", got)
	}
	if run(t, remote, "show", "main:a.txt") != "1" {
		t.Fatal("main в origin не должен меняться")
	}

	// Прямо в текущую ветку, без push.
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("3"), 0o644)
	res, err = Finalize(ctx, work, store.ProjectSettings{Commit: store.CommitDirect, Push: store.PushOff}, nil, "", "Тройка", "")
	if err != nil || res.Branch != "edy/two" || res.Pushed {
		t.Fatalf("direct: %+v %v", res, err)
	}
	if n := run(t, work, "rev-list", "--count", "main..edy/two"); n != "2" {
		t.Fatalf("коммитов в ветке: %s", n)
	}

	// Без коммита.
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("4"), 0o644)
	res, _ = Finalize(ctx, work, store.ProjectSettings{Commit: store.CommitNone}, nil, "", "x", "")
	if res.Committed || run(t, work, "status", "--short") == "" {
		t.Fatalf("none: %+v", res)
	}

	// Папка без git.
	plain := t.TempDir()
	if res, err := Finalize(ctx, plain, store.DefaultProjectSettings(), nil, "edy/x", "x", ""); err != nil || res.Committed {
		t.Fatalf("без git: %+v %v", res, err)
	}

	info := GetInfo(ctx, work)
	if !info.IsRepo || info.Branch != "edy/two" || len(info.Changed) != 1 || len(info.Log) < 3 || info.Remote != remote {
		t.Fatalf("info = %+v", info)
	}
}

func TestCloneAndPull(t *testing.T) {
	work, remote := repoWithRemote(t)
	ctx := context.Background()
	dest := filepath.Join(t.TempDir(), "clone")
	if err := Clone(ctx, remote, dest, &store.GitAccount{Kind: store.Gitea, Token: "tok"}); err != nil {
		t.Fatal(err)
	}
	if err := Clone(ctx, remote, dest, nil); err == nil {
		t.Fatal("клон в существующую папку должен отклоняться")
	}
	os.WriteFile(filepath.Join(work, "b.txt"), []byte("b"), 0o644)
	run(t, work, "add", "-A")
	run(t, work, "commit", "-q", "-m", "b")
	run(t, work, "push", "-q")
	if _, err := Pull(ctx, dest, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "b.txt")); err != nil {
		t.Fatal("pull не принёс изменения")
	}
	// Токен не должен оседать в настройках клона.
	cfg, _ := os.ReadFile(filepath.Join(dest, ".git", "config"))
	if strings.Contains(string(cfg), "Authorization") || strings.Contains(string(cfg), "tok") {
		t.Fatalf("токен попал в .git/config:\n%s", cfg)
	}
}

func TestRepoPath(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/vova/edy.git":           "vova/edy",
		"https://gitlab.example.com/group/sub/app":  "group/sub/app",
		"git@github.com:vova/edy.git":               "vova/edy",
		"https://user:pass@gitea.local/vova/x.git/": "vova/x",
	} {
		if got, err := RepoPath(in); err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	if _, err := RepoPath("https://github.com/onlyowner"); err == nil {
		t.Error("без имени репозитория должна быть ошибка")
	}
	if stripCredentials("https://user:pass@host/a/b") != "https://host/a/b" {
		t.Error("логин и пароль должны вырезаться из URL")
	}
}

func TestCreatePR(t *testing.T) {
	var got struct {
		path, auth string
		body       map[string]string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.EscapedPath()
		got.auth = r.Header.Get("Authorization") + r.Header.Get("PRIVATE-TOKEN")
		json.NewDecoder(r.Body).Decode(&got.body)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"html_url":"https://x/pr/1","web_url":"https://x/mr/1"}`))
	}))
	defer srv.Close()
	ctx := context.Background()

	u, err := CreatePR(ctx, store.GitAccount{Kind: store.GitHub, Host: srv.URL, Token: "gh"}, "https://ghe.local/vova/edy.git", "main", "edy/x", "T", "B")
	if err != nil || u != "https://x/pr/1" || got.path != "/api/v3/repos/vova/edy/pulls" || got.auth != "Bearer gh" || got.body["head"] != "edy/x" {
		t.Fatalf("github: %q %v %+v", u, err, got)
	}
	u, err = CreatePR(ctx, store.GitAccount{Kind: store.Gitea, Host: srv.URL, Token: "gt"}, "git@gitea:vova/edy.git", "main", "edy/x", "T", "B")
	if err != nil || got.path != "/api/v1/repos/vova/edy/pulls" || got.auth != "token gt" {
		t.Fatalf("gitea: %q %v %+v", u, err, got)
	}
	u, err = CreatePR(ctx, store.GitAccount{Kind: store.GitLab, Host: srv.URL, Token: "gl"}, "https://gitlab/group/app.git", "main", "edy/x", "T", "B")
	if err != nil || u != "https://x/mr/1" || got.path != "/api/v4/projects/group%2Fapp/merge_requests" || got.auth != "gl" || got.body["source_branch"] != "edy/x" {
		t.Fatalf("gitlab: %q %v %+v", u, err, got)
	}
	if _, err := CreatePR(ctx, store.GitAccount{Kind: store.GitHub}, "https://github.com/a/b", "main", "x", "T", ""); err == nil {
		t.Fatal("без токена должна быть ошибка")
	}
}
