package daemon

import (
	"testing"
	"time"

	"github.com/brushknight/agents-deck/backend/internal/cursor"
	"github.com/brushknight/agents-deck/backend/internal/model"
	"github.com/brushknight/agents-deck/backend/internal/store"
)

func TestCursorAgentsOnTheDeck(t *testing.T) {
	d := &Daemon{Store: store.New("", "test", "0"), Cfg: Config{CursorWindowHours: 24}}
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	run := cursor.Agent{
		ID: "bc_run", Name: "Fix authentication bug", Status: "RUNNING",
		Repository: "https://user:secret@git.example/acme/payments.git", Ref: "main",
		Branch: "cursor/fix-auth", URL: "https://cursor.com/agents?id=bc_run",
		Summary: "Tracing the token check", Created: now.Add(-time.Hour),
	}
	done := cursor.Agent{
		ID: "bc_done", Name: "Add a README", Status: "FINISHED",
		Repository: "https://git.example/acme/docs", Branch: "cursor/readme",
		URL: "https://cursor.com/agents?id=bc_done", PRURL: "https://git.example/acme/docs/pull/12",
		Summary: "Added install steps", Created: now.Add(-2 * time.Hour), Updated: now.Add(-30 * time.Minute),
	}
	old := cursor.Agent{
		ID: "bc_old", Name: "Old migration", Status: "FINISHED",
		Repository: "https://git.example/acme/old", Created: now.Add(-60 * 24 * time.Hour),
	}
	bad := cursor.Agent{
		ID: "bc_bad", Name: "Broken deploy", Status: "ERROR",
		Repository: "https://git.example/acme/payments", Summary: "The build failed",
		Created: now.Add(-10 * time.Minute),
	}
	ask := cursor.Agent{
		ID: "bc_ask", Name: "Needs a reviewer", Status: "NEEDS_INPUT",
		Summary: "Which approach do you want?", Created: now.Add(-5 * time.Minute),
	}

	d.syncCursor([]cursor.Agent{run, done, old, bad, ask}, now)

	id := cursor.AgentID("bc_run")
	e, ok := d.Store.Get(id)
	if !ok || e.A.Status != model.Running || e.A.App != "cursor" || e.A.Tool != "cursor" || !e.A.External ||
		e.A.Title != "Fix authentication bug" || e.A.Folder != "acme/payments" || e.A.Branch != "cursor/fix-auth" ||
		e.A.ModelLabel != "Cursor" || e.OpenURL != run.URL || e.A.LastPrompt != "Tracing the token check" ||
		e.A.Activity == nil || e.A.Activity.Detail != "acme/payments · cursor/fix-auth" ||
		stringsContains(e.A.Folder, "secret") || stringsContains(e.A.Cwd, "secret") {
		t.Fatalf("running: %+v activity %+v", e.A, e.A.Activity)
	}
	if !e.A.StatusSince.Equal(run.Created) {
		t.Fatalf("status since poll time, want last activity: %s", e.A.StatusSince)
	}
	if _, ok := d.Store.Get(cursor.AgentID("bc_old")); ok {
		t.Fatal("finished agent outside the window was shown")
	}
	fin, _ := d.Store.Get(cursor.AgentID("bc_done"))
	if fin.A.Status != model.Idle || fin.A.Unseen || fin.A.LastPrompt != done.Note() || !fin.A.StatusSince.Equal(done.Updated) {
		t.Fatalf("finished: status %s unseen %v prompt %q since %s", fin.A.Status, fin.A.Unseen, fin.A.LastPrompt, fin.A.StatusSince)
	}
	errA, _ := d.Store.Get(cursor.AgentID("bc_bad"))
	if errA.A.Status != model.Error || errA.A.Error == nil || errA.A.Error.Message != "The build failed" {
		t.Fatalf("error: %+v", errA.A)
	}
	askE, _ := d.Store.Get(cursor.AgentID("bc_ask"))
	if askE.A.Status != model.Waiting || askE.A.Waiting == nil || len(askE.A.Waiting.Options) != 0 ||
		askE.A.Waiting.Kind != "input" || askE.A.Waiting.Context != "open it in cursor" {
		t.Fatalf("needs input: %+v prompt %+v", askE.A, askE.A.Waiting)
	}
	promptID := askE.A.Waiting.ID
	d.syncCursor([]cursor.Agent{run, done, bad, ask}, now)
	askE, _ = d.Store.Get(cursor.AgentID("bc_ask"))
	if askE.A.Waiting == nil || askE.A.Waiting.ID != promptID {
		t.Fatal("prompt id changed on the same question")
	}

	// A turn that finishes while the deck is watching is hungry.
	run.Status = "FINISHED"
	run.Updated = now
	run.Summary = "Pushed the fix"
	d.syncCursor([]cursor.Agent{run, done, bad, ask}, now)
	e, _ = d.Store.Get(id)
	if e.A.Status != model.Idle || !e.A.Unseen || e.A.Activity != nil {
		t.Fatalf("finished while watched: %+v", e.A)
	}

	// Answer and interrupt don't apply. Focus opens the page.
	if err := d.Interrupt(id); err == nil || !stringsContains(err.Error(), "read-only") {
		t.Fatalf("interrupt: %v", err)
	}
	var opened string
	d.cursor.open = func(u string) error { opened = u; return nil }
	if err := d.Focus(id); err != nil || opened != run.URL {
		t.Fatalf("focus: %v opened %q", err, opened)
	}
	e, _ = d.Store.Get(id)
	if e.A.Unseen {
		t.Fatal("focus left the tile hungry")
	}
	if _, err := pageURL("javascript:alert(1)"); err == nil {
		t.Fatal("bad page was accepted")
	}
	if _, err := pageURL("https://user:secret@git.example/agent"); err == nil {
		t.Fatal("page with a secret was accepted")
	}

	// Hide until the row changes, and drop an agent that leaves the list.
	d.Dismiss(cursor.AgentID("bc_done"))
	if _, ok := d.Store.Get(cursor.AgentID("bc_done")); ok {
		t.Fatal("hide left the tile up")
	}
	d.syncCursor([]cursor.Agent{run, done, bad, ask}, now)
	if _, ok := d.Store.Get(cursor.AgentID("bc_done")); ok {
		t.Fatal("hidden agent came back unchanged")
	}
	done.Summary = "Follow-up pushed"
	d.syncCursor([]cursor.Agent{run, done, bad, ask}, now)
	if _, ok := d.Store.Get(cursor.AgentID("bc_done")); !ok {
		t.Fatal("hidden agent stayed hidden after its summary changed")
	}
	d.syncCursor(nil, now)
	if len(d.Store.Entries()) != 0 {
		t.Fatalf("agents left on the board: %d", len(d.Store.Entries()))
	}
}

func TestCursorKeyPrefersEnv(t *testing.T) {
	t.Setenv("CURSOR_API_KEY_TEST", "from-env")
	d := &Daemon{Cfg: Config{CursorAPIKey: "from-file", CursorAPIKeyEnv: "CURSOR_API_KEY_TEST"}}
	if d.cursorKey() != "from-env" {
		t.Fatalf("env should win: %q", d.cursorKey())
	}
	t.Setenv("CURSOR_API_KEY_TEST", "")
	if d.cursorKey() != "from-file" {
		t.Fatalf("file key: %q", d.cursorKey())
	}
	if d.cursorPoll() != 20*time.Second {
		t.Fatal(d.cursorPoll())
	}
	d.Cfg.CursorPollSeconds = 15
	if d.cursorPoll() != 15*time.Second {
		t.Fatal(d.cursorPoll())
	}
	d.Cfg.CursorPollSeconds = 1
	if d.cursorPoll() != 10*time.Second {
		t.Fatal(d.cursorPoll())
	}
}

func stringsContains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (s == sub || len(s) > 0 && contains(s, sub)))
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
