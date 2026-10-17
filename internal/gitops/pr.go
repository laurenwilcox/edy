package gitops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/laurenwilcox/name/internal/store"
)

// CreatePR открывает Pull Request (GitHub, Gitea) или Merge Request (GitLab)
// из ветки head в base и возвращает ссылку на него.
func CreatePR(ctx context.Context, acc store.GitAccount, remote, base, head, title, body string) (string, error) {
	if acc.Token == "" {
		return "", errors.New("нужен аккаунт с токеном")
	}
	repo, err := RepoPath(remote)
	if err != nil {
		return "", err
	}
	host := strings.TrimRight(acc.Host, "/")
	var endpoint, link string
	var payload map[string]string
	header := http.Header{}
	switch acc.Kind {
	case store.GitHub:
		api := "https://api.github.com"
		if host != "" && host != "https://github.com" {
			api = host + "/api/v3" // GitHub Enterprise
		}
		endpoint = api + "/repos/" + repo + "/pulls"
		payload = map[string]string{"title": title, "head": head, "base": base, "body": body}
		header.Set("Authorization", "Bearer "+acc.Token)
		header.Set("Accept", "application/vnd.github+json")
		link = "html_url"
	case store.Gitea:
		endpoint = host + "/api/v1/repos/" + repo + "/pulls"
		payload = map[string]string{"title": title, "head": head, "base": base, "body": body}
		header.Set("Authorization", "token "+acc.Token)
		link = "html_url"
	case store.GitLab:
		endpoint = host + "/api/v4/projects/" + url.PathEscape(repo) + "/merge_requests"
		payload = map[string]string{"title": title, "source_branch": head, "target_branch": base, "description": body}
		header.Set("PRIVATE-TOKEN", acc.Token)
		link = "web_url"
	default:
		return "", fmt.Errorf("неизвестный хостинг %q", acc.Kind)
	}

	data, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header = header
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Message string `json:"message"`
		}
		json.Unmarshal(raw, &e)
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(raw))
		}
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, e.Message)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	s, _ := out[link].(string)
	return s, nil
}
