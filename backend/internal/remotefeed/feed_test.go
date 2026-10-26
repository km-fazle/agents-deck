package remotefeed

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brushknight/agents-deck/backend/internal/model"
)

func TestExampleFeed(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "fixtures", "remote-feed.json"))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Source != "Grok" || snap.FetchedAt.Format(time.RFC3339) != "2026-10-25T12:00:00Z" || len(snap.Agents) != 5 {
		t.Fatalf("snapshot: %+v (%d agents)", snap, len(snap.Agents))
	}
	byID := map[string]Agent{}
	for _, a := range snap.Agents {
		byID[a.ID] = a
	}
	now := snap.FetchedAt.Add(30 * time.Second)
	window := 3 * time.Minute
	inbox := byID["inbox"]
	st, msg := inbox.Map()
	if st != model.Running || msg != "" || inbox.Stale(snap.FetchedAt, now, now, window) {
		t.Fatalf("inbox: %s %q stale %v", st, msg, inbox.Stale(snap.FetchedAt, now, now, window))
	}
	if inbox.Group != "ops" || inbox.OpenURL == "" || inbox.Detail() != "Reading the latest thread" {
		t.Fatalf("inbox fields: %+v", inbox)
	}
	review := byID["review"]
	st, _ = review.Map()
	if st != model.Waiting || review.Stale(snap.FetchedAt, now, now, window) {
		t.Fatalf("review: %s stale %v", st, review.Stale(snap.FetchedAt, now, now, window))
	}
	digest := byID["digest"]
	st, _ = digest.Map()
	if st != model.Idle || digest.Stale(snap.FetchedAt, now, now, window) {
		t.Fatalf("quiet idle agent should stay idle in a fresh feed: %s stale %v", st, digest.Stale(snap.FetchedAt, now, now, window))
	}
	pager := byID["pager"]
	st, msg = pager.Map()
	if st != model.Error || msg != "The mailbox refused the login" {
		t.Fatalf("pager: %s %q", st, msg)
	}
	archive := byID["archive"]
	if !archive.LooksLive() || !archive.Stale(snap.FetchedAt, now, now, window) {
		t.Fatal("working agent with an old last_activity should be stale")
	}
	// A feed the publisher marked old is stale for everyone, including idle.
	if !digest.Stale(snap.FetchedAt, now, snap.FetchedAt.Add(10*time.Minute), window) {
		t.Fatal("old fetched_at should mark the feed stale")
	}
	// A feed the deck has not been able to refresh is stale even when the
	// document's own timestamp still looks recent.
	if !inbox.Stale(snap.FetchedAt, snap.FetchedAt, snap.FetchedAt.Add(10*time.Minute), window) {
		t.Fatal("old load time should mark the feed stale")
	}
}

func TestStatusWords(t *testing.T) {
	cases := []struct {
		in   string
		want model.Status
		msg  string
	}{
		{"working", model.Running, ""},
		{"running", model.Running, ""},
		{"idle", model.Idle, ""},
		{"done", model.Idle, ""},
		{"needs_input", model.Waiting, ""},
		{"needs-attention", model.Waiting, ""},
		{"offline", model.Error, "offline"},
		{"stale", model.Error, "stale"},
		{"nope", model.Idle, ""},
	}
	for _, c := range cases {
		st, msg := (Agent{Status: c.in, Summary: "x"}).Map()
		if c.in == "error" {
			continue
		}
		if st != c.want || (c.msg != "" && msg != c.msg) {
			t.Errorf("%q -> %s %q, want %s %q", c.in, st, msg, c.want, c.msg)
		}
	}
	st, msg := (Agent{Status: "error"}).Map()
	if st != model.Error || msg != "error" {
		t.Fatalf("bare error: %s %q", st, msg)
	}
	st, msg = (Agent{Status: "error", Summary: "boom"}).Map()
	if st != model.Error || msg != "boom" {
		t.Fatalf("error summary: %s %q", st, msg)
	}
}

func TestAliasesAndSlug(t *testing.T) {
	snap, err := Parse([]byte(`{
		"source": "Ops bots",
		"fetchedAt": "2026-10-25T12:00:00Z",
		"agents": [{
			"id": "a1",
			"name": "Helper",
			"team": "desk",
			"status": "working",
			"lastMessage": "hello",
			"lastActivity": "2026-10-25T12:00:00Z",
			"openUrl": "https://example.com/a1"
		}]
	}`))
	if err != nil || len(snap.Agents) != 1 {
		t.Fatal(err)
	}
	a := snap.Agents[0]
	if a.Group != "desk" || a.Summary != "hello" || a.OpenURL == "" || snap.FetchedAt.IsZero() {
		t.Fatalf("%+v fetched %s", a, snap.FetchedAt)
	}
	if Slug("Grok") != "grok" || Slug("Ops bots") != "opsbots" || Slug("Orca") != "feed" || Slug("") != "feed" {
		t.Fatalf("slugs: %s %s %s %s", Slug("Grok"), Slug("Ops bots"), Slug("Orca"), Slug(""))
	}
	if DisplayName("") != "Grok" || DisplayName(" Ops ") != "Ops" {
		t.Fatal(DisplayName(""))
	}
	long := Slug("abcdefghijklmnopqrstuvwxyz")
	if len(long) != 11 {
		t.Fatal(long)
	}
	if AgentID("grok", "a1") == AgentID("grok", "a2") || !strings.HasPrefix(AgentID("grok", "a1"), "r") {
		t.Fatal(AgentID("grok", "a1"))
	}
}

func TestLoadFileAndURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "feed.json")
	body := []byte(`{"source":"Grok","agents":[{"id":"inbox","status":"idle","name":"Inbox"}]}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := (Source{Path: path}).Load(context.Background())
	if err != nil || len(snap.Agents) != 1 || snap.Agents[0].Name != "Inbox" {
		t.Fatalf("file: %v %+v", err, snap)
	}

	const token = "feed-token-not-real"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("authorization: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Retry-After", "12")
		http.Error(w, "nope "+token, http.StatusTooManyRequests)
	}))
	defer srv.Close()
	_, err = (Source{URL: srv.URL, Token: token, HTTP: srv.Client()}).Load(context.Background())
	var api *APIError
	if !errors.As(err, &api) || api.Status != 429 || api.RetryAfter != "12" || strings.Contains(err.Error(), token) {
		t.Fatalf("url error: %v", err)
	}
	if err := CheckURL("http://192.0.2.1/feed.json"); err == nil {
		t.Fatal("plain http to a non-loopback host was accepted")
	}
	if err := CheckURL("https://user:secret@192.0.2.1/feed.json"); err == nil {
		t.Fatal("a url with a username was accepted")
	}
	if err := CheckURL(srv.URL); err != nil {
		t.Fatal(err)
	}
}

func TestFeedBackoff(t *testing.T) {
	base := 20 * time.Second
	if got := Backoff(base, base, &APIError{Status: 429, RetryAfter: "15"}); got != 15*time.Second {
		t.Fatal(got)
	}
	if got := Backoff(base, base, &APIError{Status: 500}); got != 40*time.Second {
		t.Fatal(got)
	}
	if got := Backoff(base, base, errors.New("missing file")); got != base {
		t.Fatal(got)
	}
}
