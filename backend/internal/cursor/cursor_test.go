package cursor

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/brushknight/agents-deck/backend/internal/model"
)

const listPage = `{
  "agents": [
    {
      "id": "bc_run",
      "name": "Fix authentication bug",
      "status": "RUNNING",
      "source": {"repository": "https://user:secret@git.example/acme/payments.git", "ref": "main"},
      "target": {
        "branchName": "cursor/fix-auth-5678",
        "url": "https://cursor.com/agents?id=bc_run",
        "autoCreatePr": true
      },
      "summary": "Tracing the token check",
      "createdAt": "2026-10-25T11:00:00Z"
    },
    {
      "id": "bc_done",
      "name": "Add README Documentation",
      "status": "FINISHED",
      "source": {"repository": "https://git.example/acme/docs", "ref": "main"},
      "target": {
        "branchName": "cursor/add-readme-1234",
        "url": "https://cursor.com/agents?id=bc_done",
        "prUrl": "https://git.example/acme/docs/pull/1234"
      },
      "summary": "Added a README with install steps",
      "createdAt": "2026-10-25T10:30:00Z",
      "updatedAt": "2026-10-25T11:45:00Z"
    },
    {
      "id": "bc_old",
      "name": "Old migration",
      "status": "FINISHED",
      "source": {"repository": "https://git.example/acme/old"},
      "target": {"branchName": "cursor/old"},
      "createdAt": "2026-09-01T10:30:00Z"
    },
    {
      "id": "bc_bad",
      "name": "Broken deploy",
      "status": "ERROR",
      "source": {"repository": "https://git.example/acme/payments"},
      "summary": "The build failed",
      "createdAt": "2026-10-25T11:50:00Z"
    },
    {
      "id": "bc_new",
      "name": "Starting up",
      "status": "CREATING",
      "createdAt": "2026-10-25T11:59:00Z"
    },
    {
      "id": "bc_ask",
      "name": "Needs a reviewer",
      "status": "NEEDS_INPUT",
      "summary": "Which approach do you want?",
      "createdAt": "2026-10-25T11:40:00Z"
    }
  ],
  "nextCursor": ""
}`

func TestMapStatus(t *testing.T) {
	cases := []struct {
		in   string
		want model.Status
	}{
		{"CREATING", model.Starting},
		{"RUNNING", model.Running},
		{"ACTIVE", model.Running},
		{"FINISHED", model.Idle},
		{"IDLE", model.Idle},
		{"CANCELLED", model.Idle},
		{"EXPIRED", model.Idle},
		{"ARCHIVED", model.Idle},
		{"ERROR", model.Error},
		{"FAILED", model.Error},
		{"NEEDS_INPUT", model.Waiting},
		{"WAITING_FOR_USER", model.Waiting},
		{"BLOCKED", model.Waiting},
		{"MYSTERY", model.Idle},
		{"", model.Idle},
	}
	for _, c := range cases {
		if got := MapStatus(c.in); got != c.want {
			t.Errorf("MapStatus(%q) = %s, want %s", c.in, got, c.want)
		}
	}
	if Live("RUNNING") != true || Live("FINISHED") != false || Live("MYSTERY") != false {
		t.Fatal("Live mis-classified a status")
	}
}

func TestParseListAndFields(t *testing.T) {
	list, next, err := ParseList([]byte(listPage))
	if err != nil || next != "" || len(list) != 6 {
		t.Fatalf("parse: %v next %q len %d", err, next, len(list))
	}
	run := list[0]
	if run.RepoLabel() != "acme/payments" || strings.Contains(run.Repository, "secret") || strings.Contains(run.RepoLabel(), "secret") {
		t.Fatalf("repo label leaked or wrong: %q / %q", run.Repository, run.RepoLabel())
	}
	if run.BranchName() != "cursor/fix-auth-5678" || run.Detail() != "acme/payments · cursor/fix-auth-5678" {
		t.Fatalf("branch/detail: %q %q", run.BranchName(), run.Detail())
	}
	if run.PageURL() != "https://cursor.com/agents?id=bc_run" || run.Note() != "Tracing the token check" {
		t.Fatalf("page/note: %q %q", run.PageURL(), run.Note())
	}
	done := list[1]
	if done.Note() != "Added a README with install steps\nhttps://git.example/acme/docs/pull/1234" {
		t.Fatalf("pr note: %q", done.Note())
	}
	if done.When().Format(time.RFC3339) != "2026-10-25T11:45:00Z" {
		t.Fatalf("when uses updatedAt: %s", done.When())
	}
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	window := 24 * time.Hour
	if !run.Include(now, window) || !done.Include(now, window) || list[2].Include(now, window) {
		t.Fatalf("window: run %v done %v old %v", run.Include(now, window), done.Include(now, window), list[2].Include(now, window))
	}
	// A long-running agent stays even when created outside the window.
	oldRun := list[2]
	oldRun.Status = "RUNNING"
	if !oldRun.Include(now, window) {
		t.Fatal("running agent outside the window was dropped")
	}
	if list[3].ErrorText() != "The build failed" {
		t.Fatalf("error text: %q", list[3].ErrorText())
	}
	bare := Agent{ID: "bc_x"}
	if bare.PageURL() != "https://cursor.com/agents?id=bc_x" || bare.ErrorText() != "cloud agent error" {
		t.Fatalf("defaults: %q %q", bare.PageURL(), bare.ErrorText())
	}
	if AgentID("bc_run") == AgentID("bc_done") || !strings.HasPrefix(AgentID("bc_run"), "c") || len(AgentID("bc_run")) != 6 {
		t.Fatalf("id: %s", AgentID("bc_run"))
	}
}

func TestParseRejectsV1List(t *testing.T) {
	_, _, err := ParseList([]byte(`{"items":[{"id":"bc-1","status":"ACTIVE"}]}`))
	if err == nil || !strings.Contains(err.Error(), "/v0/agents") {
		t.Fatalf("v1 list: %v", err)
	}
}

func TestClientPagesAuthAndRedaction(t *testing.T) {
	const key = "test-key-not-real"
	var sawCursor string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != key || pass != "" {
			t.Errorf("basic auth = %q %q ok %v", user, pass, ok)
		}
		raw := r.Header.Get("Authorization")
		dec, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(raw, "Basic "))
		if err != nil || string(dec) != key+":" {
			t.Errorf("authorization header: %q", raw)
		}
		switch r.URL.Query().Get("cursor") {
		case "":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"agents":[{"id":"bc_one","status":"RUNNING","name":"One"}],"nextCursor":"bc_two"}`))
		case "bc_two":
			sawCursor = "bc_two"
			_, _ = w.Write([]byte(`{"agents":[{"id":"bc_two","status":"FINISHED","createdAt":"2026-10-25T11:00:00Z"}]}`))
		default:
			t.Errorf("unexpected cursor %q", r.URL.Query().Get("cursor"))
			http.Error(w, "no", 500)
		}
	}))
	defer srv.Close()

	c := Client{BaseURL: srv.URL, Key: key, HTTP: srv.Client()}
	list, err := c.Agents(context.Background())
	if err != nil || len(list) != 2 || sawCursor != "bc_two" {
		t.Fatalf("list: %v (%d) cursor %q", err, len(list), sawCursor)
	}

	// A 401 body that echoes the key must not come back in the error.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rejected "+key, http.StatusUnauthorized)
	}))
	defer bad.Close()
	c.BaseURL = bad.URL
	_, err = c.Agents(context.Background())
	var api *APIError
	if !errors.As(err, &api) || api.Status != 401 || strings.Contains(err.Error(), key) {
		t.Fatalf("401 error: %v", err)
	}

	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "slow down "+key, http.StatusTooManyRequests)
	}))
	defer limited.Close()
	c.BaseURL = limited.URL
	_, err = c.Agents(context.Background())
	if !errors.As(err, &api) || api.Status != 429 || api.RetryAfter != "30" || strings.Contains(err.Error(), key) {
		t.Fatalf("429 error: %#v %v", api, err)
	}
}

func TestClientRefusesOtherHosts(t *testing.T) {
	const key = "test-key-not-real"
	_, err := (Client{BaseURL: "https://192.0.2.1", Key: key}).Agents(context.Background())
	if err == nil || strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "api.cursor.com") {
		t.Fatalf("other host: %v", err)
	}
	if err := CheckBase(""); err != nil {
		t.Fatal(err)
	}
	if err := CheckBase("https://user:pw@api.cursor.com"); err == nil {
		t.Fatal("userinfo was accepted")
	}
	if err := CheckBase("http://api.cursor.com"); err == nil {
		t.Fatal("cleartext was accepted")
	}
}

func TestBackoff(t *testing.T) {
	base := 20 * time.Second
	if got := Backoff(base, base, errors.New("no key")); got != base {
		t.Fatalf("plain error: %s", got)
	}
	if got := Backoff(base, base, &APIError{Status: 401}); got != base {
		t.Fatalf("401: %s", got)
	}
	if got := Backoff(base, base, &APIError{Status: 429, RetryAfter: "45"}); got != 45*time.Second {
		t.Fatalf("retry-after: %s", got)
	}
	if got := Backoff(base, base, &APIError{Status: 503}); got != 40*time.Second {
		t.Fatalf("first 503: %s", got)
	}
	if got := Backoff(80*time.Second, base, &APIError{Status: 500}); got != 160*time.Second {
		t.Fatalf("second 500: %s", got)
	}
	if got := Backoff(4*time.Minute, base, &APIError{Status: 502}); got != 5*time.Minute {
		t.Fatalf("cap: %s", got)
	}
}

func TestNoKey(t *testing.T) {
	_, err := (Client{}).Agents(context.Background())
	if err == nil || strings.Contains(err.Error(), "http") {
		t.Fatal(err)
	}
}
