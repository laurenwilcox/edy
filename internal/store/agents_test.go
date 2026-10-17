package store

import (
	"bytes"
	"testing"
	"time"
)

func TestAgentJournal(t *testing.T) {
	s := openTest(t)
	if err := s.CreateAgent(Agent{ID: "a1", Role: RoleExecutor, Model: "agy/m", Project: "/p", TaskID: "t1", Goal: "почини"}); err != nil {
		t.Fatal(err)
	}
	s.CreateAgent(Agent{ID: "a2", Role: RoleOrchestrator, Model: "agy/m", Created: time.Now().Add(time.Second)})
	s.AddAgentEvent("a1", "tool", "read_file calc.go", time.Now())
	s.AddAgentEvent("a1", "hooks", "✓ go test", time.Now())

	list, err := s.Agents(10)
	if err != nil || len(list) != 2 || list[0].ID != "a2" || list[1].Events != 2 || list[1].State != AgentRunning {
		t.Fatalf("list = %+v, %v", list, err)
	}
	ev, _ := s.AgentEvents("a1", 0)
	if len(ev) != 2 || ev[1].Text != "✓ go test" {
		t.Fatalf("events = %+v", ev)
	}
	if tail, _ := s.AgentEvents("a1", ev[0].ID); len(tail) != 1 {
		t.Fatalf("tail = %+v", tail)
	}

	s.FinishAgent("a1", AgentDone, "Исправил", "")
	if a, _ := s.Agent("a1"); a.State != AgentDone || a.Finished.IsZero() || a.Summary != "Исправил" {
		t.Fatalf("a1 = %+v", a)
	}
	s.TouchAgent("a2", AgentDone, "ответил")
	if a, _ := s.Agent("a2"); a.Finished.IsZero() {
		t.Fatal("после ответа должно ставиться время окончания")
	}
	s.TouchAgent("a2", AgentRunning, "")
	if a, _ := s.Agent("a2"); !a.Finished.IsZero() {
		t.Fatal("при новой работе время окончания сбрасывается")
	}
	if n, _ := s.InterruptRunningAgents(); n != 1 {
		t.Fatalf("прервано %d, ожидался 1 (a2)", n)
	}
	if a, _ := s.Agent("a2"); a.State != AgentInterrupted {
		t.Fatalf("a2 = %+v", a)
	}
	if _, err := s.Agent("nope"); err != ErrNotFound {
		t.Fatal(err)
	}
}

func TestGitAccountsAndProjectSettings(t *testing.T) {
	s := openTest(t)
	id, err := s.AddGitAccount(GitAccount{Kind: GitHub, Host: "https://github.com", Label: "личный", Token: "ghp_secret"})
	if err != nil {
		t.Fatal(err)
	}
	var raw []byte
	s.db.QueryRow(`SELECT token FROM git_accounts WHERE id = ?`, id).Scan(&raw)
	if bytes.Contains(raw, []byte("ghp_secret")) {
		t.Fatal("токен хранится открытым текстом")
	}
	list, _ := s.GitAccounts()
	if len(list) != 1 || list[0].Token != "" {
		t.Fatalf("в списке токен не должен расшифровываться: %+v", list)
	}
	if a, _ := s.GitAccount(id); a.Token != "ghp_secret" {
		t.Fatalf("token = %q", a.Token)
	}

	ps, _ := s.ProjectSettings("/p")
	if ps != DefaultProjectSettings() {
		t.Fatalf("по умолчанию: %+v", ps)
	}
	ps.Commit, ps.Push, ps.AccountID = CommitDirect, PushOn, id
	s.SetProjectSettings("/p", ps)
	if got, _ := s.ProjectSettings("/p"); got != ps {
		t.Fatalf("got = %+v", got)
	}
	if err := s.DeleteGitAccount(id); err != nil {
		t.Fatal(err)
	}
}
