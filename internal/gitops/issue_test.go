package gitops

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/laurenwilcox/name/internal/store"
)

func TestParseIssueURL(t *testing.T) {
	accs := []store.GitAccount{{Kind: store.GitHub, Host: "https://ghe.corp"}}
	for raw, want := range map[string]IssueRef{
		"https://github.com/vova/app/issues/12":      {store.GitHub, "https://github.com", "vova/app", 12},
		"https://gitlab.com/team/sub/app/-/issues/7": {store.GitLab, "https://gitlab.com", "team/sub/app", 7},
		"https://git.home/vova/app/issues/3":         {store.Gitea, "https://git.home", "vova/app", 3},
		"https://ghe.corp/vova/app/issues/5/":        {store.GitHub, "https://ghe.corp", "vova/app", 5},
	} {
		got, err := ParseIssueURL(raw, accs)
		if err != nil || got != want {
			t.Errorf("%s: %+v %v", raw, got, err)
		}
	}
	for _, bad := range []string{"github.com/x/y/issues/1", "https://github.com/x/y/pull/1", "file:///etc/passwd"} {
		if _, err := ParseIssueURL(bad, nil); err == nil {
			t.Errorf("%s должна отклоняться", bad)
		}
	}
}

func TestFetchIssueSendsTokenOnlyToItsHost(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/api/v1/repos/vova/app/issues/3" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"number":3,"title":"Кнопка не работает","body":"Шаги…","html_url":"` + "http://x/3" + `","labels":[{"name":"bug"}]}`))
	}))
	defer srv.Close()
	ref := IssueRef{Kind: store.Gitea, Host: srv.URL, Repo: "vova/app", Number: 3}
	is, err := FetchIssue(context.Background(), ref, &store.GitAccount{Host: srv.URL, Token: "secret"}, srv.Client())
	if err != nil || is.Title != "Кнопка не работает" || is.Labels[0] != "bug" || auth != "token secret" {
		t.Fatalf("issue: %+v %v auth=%q", is, err, auth)
	}
	is, err = FetchIssue(context.Background(), ref, &store.GitAccount{Host: "https://other.host", Token: "secret"}, srv.Client())
	if err != nil || auth != "" {
		t.Fatalf("токен чужого хоста не отправляется: %q %v", auth, err)
	}
	ref.Number = 99
	if _, err := FetchIssue(context.Background(), ref, nil, srv.Client()); err == nil || !strings.Contains(err.Error(), "не найден") {
		t.Fatalf("404: %v", err)
	}
}
