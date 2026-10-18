package gitops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/laurenwilcox/name/internal/store"
)

// Issue — задача из GitHub, GitLab или Gitea.
type Issue struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	URL    string   `json:"url"`
	Labels []string `json:"labels,omitempty"`
}

var (
	gitlabIssueRe = regexp.MustCompile(`^/(.+?)/-/issues/(\d+)/?$`)
	issueRe       = regexp.MustCompile(`^/([^/]+/[^/]+)/issues/(\d+)/?$`)
)

// IssueRef — разобранная ссылка на issue.
type IssueRef struct {
	Kind   string // github, gitlab, gitea
	Host   string // https://github.com
	Repo   string // owner/repo или group/sub/project
	Number int
}

// ParseIssueURL разбирает ссылку на issue. Вид хостинга берётся из аккаунта
// для этого адреса, если он есть, иначе угадывается по адресу.
func ParseIssueURL(raw string, accounts []store.GitAccount) (IssueRef, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return IssueRef{}, errors.New("нужна ссылка вида https://github.com/владелец/репозиторий/issues/12")
	}
	ref := IssueRef{Host: u.Scheme + "://" + u.Host}
	if m := gitlabIssueRe.FindStringSubmatch(u.Path); m != nil {
		ref.Kind, ref.Repo = store.GitLab, m[1]
		ref.Number, _ = strconv.Atoi(m[2])
		return ref, nil
	}
	m := issueRe.FindStringSubmatch(u.Path)
	if m == nil {
		return IssueRef{}, errors.New("это не ссылка на issue: ожидается …/владелец/репозиторий/issues/12 или …/-/issues/12 для GitLab")
	}
	ref.Repo = m[1]
	ref.Number, _ = strconv.Atoi(m[2])
	switch {
	case u.Host == "github.com":
		ref.Kind = store.GitHub
	default:
		ref.Kind = store.Gitea
		for _, a := range accounts {
			if strings.TrimRight(a.Host, "/") == ref.Host {
				ref.Kind = a.Kind // GitHub Enterprise или Gitea
			}
		}
	}
	return ref, nil
}

// FetchIssue скачивает issue. Токен аккаунта отправляется, только если
// аккаунт привязан к этому же адресу.
func FetchIssue(ctx context.Context, ref IssueRef, acc *store.GitAccount, client *http.Client) (Issue, error) {
	var endpoint string
	header := http.Header{}
	token := ""
	if acc != nil && strings.TrimRight(acc.Host, "/") == ref.Host {
		token = acc.Token
	}
	switch ref.Kind {
	case store.GitHub:
		api := "https://api.github.com"
		if ref.Host != "https://github.com" {
			api = ref.Host + "/api/v3"
		}
		endpoint = fmt.Sprintf("%s/repos/%s/issues/%d", api, ref.Repo, ref.Number)
		header.Set("Accept", "application/vnd.github+json")
		if token != "" {
			header.Set("Authorization", "Bearer "+token)
		}
	case store.Gitea:
		endpoint = fmt.Sprintf("%s/api/v1/repos/%s/issues/%d", ref.Host, ref.Repo, ref.Number)
		if token != "" {
			header.Set("Authorization", "token "+token)
		}
	case store.GitLab:
		endpoint = fmt.Sprintf("%s/api/v4/projects/%s/issues/%d", ref.Host, url.PathEscape(ref.Repo), ref.Number)
		if token != "" {
			header.Set("PRIVATE-TOKEN", token)
		}
	default:
		return Issue{}, fmt.Errorf("неизвестный хостинг %q", ref.Kind)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Issue{}, err
	}
	req.Header = header
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Issue{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized {
		return Issue{}, fmt.Errorf("issue не найден или нет доступа (HTTP %d) — для приватного репозитория добавьте аккаунт во вкладке Git", resp.StatusCode)
	}
	if resp.StatusCode/100 != 2 {
		return Issue{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var data struct {
		Number      int    `json:"number"`
		IID         int    `json:"iid"`
		Title       string `json:"title"`
		Body        string `json:"body"`
		Description string `json:"description"`
		HTMLURL     string `json:"html_url"`
		WebURL      string `json:"web_url"`
		Labels      []any  `json:"labels"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return Issue{}, fmt.Errorf("неожиданный ответ: %w", err)
	}
	is := Issue{Number: data.Number, Title: data.Title, Body: data.Body, URL: data.HTMLURL}
	if ref.Kind == store.GitLab {
		is.Number, is.Body, is.URL = data.IID, data.Description, data.WebURL
	}
	for _, l := range data.Labels {
		switch v := l.(type) {
		case string:
			is.Labels = append(is.Labels, v)
		case map[string]any:
			if name, ok := v["name"].(string); ok {
				is.Labels = append(is.Labels, name)
			}
		}
	}
	if is.Title == "" {
		return Issue{}, errors.New("в ответе нет названия issue")
	}
	return is, nil
}
