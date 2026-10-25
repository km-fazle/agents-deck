package daemon

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/brushknight/agents-deck/backend/internal/cursor"
	"github.com/brushknight/agents-deck/backend/internal/model"
	"github.com/brushknight/agents-deck/backend/internal/store"
)

// cursorWatch mirrors Cursor cloud agents onto the board. Focus opens the
// agent's page. Answer and interrupt don't apply: the agent runs in the
// cloud, and the deck only shows it.
type cursorWatch struct {
	mu     sync.Mutex
	hidden map[string]string // deck id -> fingerprint at hide
	list   func(context.Context) ([]cursor.Agent, error)
	open   func(string) error
}

// WatchCursor polls the Cloud Agents API. It returns at once when config.json
// has "cursor": false, or when no API key is set. The poll is 20 seconds
// unless cursorPollSeconds says otherwise (never under 10), and longer after
// a 429 or a 5xx. The key is read from the environment (CURSOR_API_KEY, or
// cursorApiKeyEnv) and otherwise from cursorApiKey. It is not logged.
func (d *Daemon) WatchCursor() {
	if d.Cfg.Cursor != nil && !*d.Cfg.Cursor {
		return
	}
	key := d.cursorKey()
	if key == "" {
		env := d.Cfg.CursorAPIKeyEnv
		if strings.TrimSpace(env) == "" {
			env = "CURSOR_API_KEY"
		}
		log.Printf("cursor: no API key (set %s, or cursorApiKey in config.json)", env)
		return
	}
	client := cursor.Client{BaseURL: strings.TrimSpace(d.Cfg.CursorBaseURL), Key: key, HTTP: cursor.HTTPClient()}
	base := d.cursorPoll()
	wait := base
	logged := ""
	for {
		list, err := d.cursorAgents(client)
		if err != nil {
			wait = cursor.Backoff(wait, base, err)
			if err.Error() != logged {
				log.Printf("cursor: %s", err.Error())
				logged = err.Error()
			}
		} else {
			logged = ""
			wait = base
			d.syncCursor(list, time.Now())
		}
		time.Sleep(wait)
	}
}

func (d *Daemon) cursorAgents(c cursor.Client) ([]cursor.Agent, error) {
	d.cursor.mu.Lock()
	f := d.cursor.list
	d.cursor.mu.Unlock()
	if f != nil {
		return f(context.Background())
	}
	return c.Agents(context.Background())
}

func (d *Daemon) cursorKey() string {
	env := d.Cfg.CursorAPIKeyEnv
	if strings.TrimSpace(env) == "" {
		env = "CURSOR_API_KEY"
	}
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v
	}
	return strings.TrimSpace(d.Cfg.CursorAPIKey)
}

func (d *Daemon) cursorPoll() time.Duration {
	return pollEvery(d.Cfg.CursorPollSeconds, 20)
}

func (d *Daemon) cursorWindow() time.Duration {
	if d.Cfg.CursorWindowHours < 0 {
		return 0
	}
	if d.Cfg.CursorWindowHours > 0 {
		return time.Duration(d.Cfg.CursorWindowHours * float64(time.Hour))
	}
	return 24 * time.Hour
}

// syncCursor puts the cloud agents that are running, or finished inside the
// window, on the board and takes off the ones that are gone.
func (d *Daemon) syncCursor(list []cursor.Agent, now time.Time) {
	d.cursor.mu.Lock()
	hidden := d.cursor.hidden
	d.cursor.mu.Unlock()
	window := d.cursorWindow()
	want := map[string]cursor.Agent{}
	for _, a := range list {
		if a.ID == "" || !a.Include(now, window) {
			continue
		}
		id := cursor.AgentID(a.ID)
		if h, ok := hidden[id]; ok && h == a.Fingerprint() {
			continue
		}
		want[id] = a
	}
	for _, e := range d.Store.Entries() {
		if e.A.App == "cursor" {
			if _, ok := want[e.A.ID]; !ok {
				d.Store.Remove(e.A.ID)
			}
		}
	}
	for id, a := range want {
		if _, ok := d.Store.Get(id); !ok {
			e := &store.Entry{External: true}
			applyCursor(e, a)
			d.Store.Add(e) // stamps statusSince with the wall clock; apply pulls it back
		}
		d.Store.Update(id, func(e *store.Entry) bool { return applyCursor(e, a) })
	}
}

func applyCursor(e *store.Entry, a cursor.Agent) bool {
	prev := e.A.Status
	next := cursor.MapStatus(a.Status)
	changed := stampStatus(e, next, a.When())
	e.External = true
	e.OpenURL = a.PageURL()
	token := a.Fingerprint()
	prevToken := e.RemoteToken
	if e.RemoteToken != token {
		e.RemoteToken = token
		changed = true
	}
	title := a.Name
	if title == "" {
		title = a.RepoLabel()
	}
	if title == "" {
		title = "Cursor agent"
	}
	repo := a.RepoLabel()
	changed = setString(&e.A.ID, cursor.AgentID(a.ID)) || changed
	changed = setString(&e.A.Title, title) || changed
	changed = setString(&e.A.Tool, "cursor") || changed
	changed = setString(&e.A.App, "cursor") || changed
	changed = setString(&e.A.ModelLabel, "Cursor") || changed
	changed = setString(&e.A.Folder, repo) || changed
	changed = setString(&e.A.Cwd, repo) || changed
	changed = setString(&e.A.Branch, a.BranchName()) || changed
	changed = setString(&e.A.LastPrompt, a.Note()) || changed
	e.A.External = true
	if at := a.When(); !at.IsZero() && e.A.StatusSince.After(at) {
		// Add() stamps statusSince with the poll time. The tile should show
		// the agent's last activity, which is older than that.
		e.A.StatusSince = at.UTC()
		changed = true
	}
	if !a.Created.IsZero() && !e.A.StartedAt.Equal(a.Created.UTC()) {
		e.A.StartedAt = a.Created.UTC()
		changed = true
	}
	if next == model.Running || next == model.Starting {
		act := &model.Activity{Tool: "cloud", Detail: a.Detail()}
		if !activityEqual(act, e.A.Activity) {
			e.A.Activity = act
			changed = true
		}
		if e.A.Unseen {
			e.A.Unseen = false
			changed = true
		}
	}
	if next == model.Error {
		msg := a.ErrorText()
		if e.A.Error == nil || e.A.Error.Message != msg {
			e.A.Error = &model.ErrInfo{Message: msg}
			changed = true
		}
	}
	if next == model.Waiting && (e.A.Waiting == nil || prevToken != token) {
		e.A.Waiting = &model.Prompt{
			ID: e.NextPromptID(), Kind: "input", Title: "needs you",
			Detail: a.Summary, Context: "open it in cursor",
		}
		changed = true
	}
	if next == model.Idle && (prev == model.Running || prev == model.Waiting || prev == model.Starting) {
		e.A.Unseen = true
		changed = true
	}
	return changed
}

func stampStatus(e *store.Entry, s model.Status, at time.Time) bool {
	changed := false
	if e.A.Status != s {
		e.A.Status = s
		if at.IsZero() {
			at = time.Now().UTC()
		}
		e.A.StatusSince = at.UTC()
		changed = true
	}
	if s != model.Waiting && e.A.Waiting != nil {
		e.A.Waiting = nil
		changed = true
	}
	if s != model.Error && e.A.Error != nil {
		e.A.Error = nil
		changed = true
	}
	if s != model.Running && s != model.Starting && e.A.Activity != nil {
		e.A.Activity = nil
		changed = true
	}
	return changed
}

func setString(dst *string, v string) bool {
	if *dst == v {
		return false
	}
	*dst = v
	return true
}

func pollEvery(seconds, fallback int) time.Duration {
	if seconds <= 0 {
		seconds = fallback
	}
	if seconds < 10 {
		seconds = 10
	}
	return time.Duration(seconds) * time.Second
}

// ---- actions --------------------------------------------------------------

func (d *Daemon) cursorSend(store.Entry, string) error {
	return errors.New("this agent is read-only on the deck — open it in cursor to answer it")
}

func (d *Daemon) cursorInterrupt(store.Entry) error {
	return errors.New("this agent is read-only on the deck — open it in cursor to stop it")
}

// cursorFocus opens the agent's page on cursor.com.
func (d *Daemon) cursorFocus(e store.Entry) error {
	d.Store.Update(e.A.ID, func(x *store.Entry) bool {
		was := x.A.Unseen
		x.A.Unseen = false
		return was
	})
	open := d.cursor.open
	if open == nil {
		open = openPage
	}
	if err := open(e.OpenURL); err != nil {
		return err
	}
	log.Printf("focus %s (cursor): opened its page", e.A.ID)
	return nil
}

// cursorHide keeps a cloud agent off the board until its status changes.
func (d *Daemon) cursorHide(e store.Entry) {
	d.cursor.mu.Lock()
	if d.cursor.hidden == nil {
		d.cursor.hidden = map[string]string{}
	}
	d.cursor.hidden[e.A.ID] = e.RemoteToken
	d.cursor.mu.Unlock()
	d.Store.Remove(e.A.ID)
}
