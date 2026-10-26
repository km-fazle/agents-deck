package daemon

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/brushknight/agents-deck/backend/internal/model"
	"github.com/brushknight/agents-deck/backend/internal/remotefeed"
	"github.com/brushknight/agents-deck/backend/internal/store"
)

// feedWatch mirrors agents from a JSON status feed (Grok bots, or anything
// else that publishes the same document). Focus opens open_url. The feed
// can't take an answer or an interrupt.
type feedWatch struct {
	mu     sync.Mutex
	hidden map[string]string
	load   func(context.Context) (remotefeed.Snapshot, error)
	open   func(string) error
	snap   remotefeed.Snapshot
	loaded time.Time
	have   bool
}

// WatchRemoteFeed polls the feed in config.json "remoteFeed". It returns at
// once when that block is missing, disabled, or has neither a url nor a path.
// The poll is 20 seconds (remoteFeed.pollSeconds, never under 10). A feed
// the deck cannot refresh, or a working agent whose last_activity is older
// than remoteFeed.staleSeconds, shows as stale instead of working.
func (d *Daemon) WatchRemoteFeed() {
	if !d.feedOn() {
		return
	}
	base := d.feedPoll()
	wait := base
	logged := ""
	for {
		snap, err := d.feedLoad()
		now := time.Now()
		if err != nil {
			wait = remotefeed.Backoff(wait, base, err)
			if err.Error() != logged {
				log.Printf("remote feed: %s", err.Error())
				logged = err.Error()
			}
			d.feed.mu.Lock()
			have, cached, loaded := d.feed.have, d.feed.snap, d.feed.loaded
			d.feed.mu.Unlock()
			if have {
				label, slug := d.feedNames(cached)
				d.syncFeed(label, slug, cached, loaded, now)
			}
		} else {
			logged = ""
			wait = base
			d.feed.mu.Lock()
			d.feed.snap, d.feed.loaded, d.feed.have = snap, now, true
			d.feed.mu.Unlock()
			label, slug := d.feedNames(snap)
			d.syncFeed(label, slug, snap, now, now)
		}
		time.Sleep(wait)
	}
}

func (d *Daemon) feedOn() bool {
	c := d.Cfg.RemoteFeed
	if c == nil || (c.Enabled != nil && !*c.Enabled) {
		return false
	}
	return strings.TrimSpace(c.URL) != "" || strings.TrimSpace(c.Path) != ""
}

func (d *Daemon) feedLoad() (remotefeed.Snapshot, error) {
	d.feed.mu.Lock()
	f := d.feed.load
	d.feed.mu.Unlock()
	if f != nil {
		return f(context.Background())
	}
	c := d.Cfg.RemoteFeed
	src := remotefeed.Source{HTTP: remotefeed.HTTPClient()}
	if c != nil {
		src.URL = strings.TrimSpace(c.URL)
		src.Path = strings.TrimSpace(c.Path)
		src.Token = c.bearer()
	}
	return src.Load(context.Background())
}

func (c *RemoteFeedConfig) bearer() string {
	if c == nil {
		return ""
	}
	env := c.TokenEnv
	if strings.TrimSpace(env) == "" {
		env = "REMOTE_FEED_TOKEN"
	}
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v
	}
	return strings.TrimSpace(c.Token)
}

func (d *Daemon) feedPoll() time.Duration {
	seconds := 0
	if d.Cfg.RemoteFeed != nil {
		seconds = d.Cfg.RemoteFeed.PollSeconds
	}
	return pollEvery(seconds, 20)
}

func (d *Daemon) feedStaleWindow() time.Duration {
	if d.Cfg.RemoteFeed == nil {
		return 3 * time.Minute
	}
	s := d.Cfg.RemoteFeed.StaleSeconds
	if s < 0 {
		return 0
	}
	if s == 0 {
		return 3 * time.Minute
	}
	return time.Duration(s) * time.Second
}

// feedNames is the card label and the short app name. The config name wins;
// otherwise the document's source; otherwise "Grok".
func (d *Daemon) feedNames(snap remotefeed.Snapshot) (label, slug string) {
	name := ""
	if d.Cfg.RemoteFeed != nil {
		name = strings.TrimSpace(d.Cfg.RemoteFeed.Name)
	}
	if name == "" {
		name = snap.Source
	}
	label = remotefeed.DisplayName(name)
	return label, remotefeed.Slug(label)
}

// syncFeed puts the feed's agents on the board and takes off the ones that
// left the document.
func (d *Daemon) syncFeed(label, slug string, snap remotefeed.Snapshot, loaded, now time.Time) {
	d.feed.mu.Lock()
	hidden := d.feed.hidden
	d.feed.mu.Unlock()
	window := d.feedStaleWindow()
	want := map[string]remotefeed.Agent{}
	for _, a := range snap.Agents {
		if a.ID == "" {
			continue
		}
		id := remotefeed.AgentID(slug, a.ID)
		if h, ok := hidden[id]; ok && h == feedToken(a, snap.FetchedAt, loaded, now, window) {
			continue
		}
		want[id] = a
	}
	for _, e := range d.Store.Entries() {
		if e.FeedSource == slug {
			if _, ok := want[e.A.ID]; !ok {
				d.Store.Remove(e.A.ID)
			}
		}
	}
	for id, a := range want {
		if _, ok := d.Store.Get(id); !ok {
			e := &store.Entry{External: true, FeedSource: slug}
			applyFeed(e, a, label, slug, snap.FetchedAt, loaded, now, window)
			d.Store.Add(e) // stamps statusSince with the wall clock; apply pulls it back
		}
		d.Store.Update(id, func(e *store.Entry) bool {
			return applyFeed(e, a, label, slug, snap.FetchedAt, loaded, now, window)
		})
	}
}

func applyFeed(e *store.Entry, a remotefeed.Agent, label, slug string, docFetched, loaded, now time.Time, window time.Duration) bool {
	prev := e.A.Status
	next, msg := a.Map()
	if a.Stale(docFetched, loaded, now, window) {
		next, msg = model.Error, "stale"
	}
	when := a.When(docFetched)
	changed := stampStatus(e, next, when)
	e.External = true
	e.FeedSource = slug
	e.OpenURL = a.OpenURL
	token := feedToken(a, docFetched, loaded, now, window)
	prevToken := e.RemoteToken
	if e.RemoteToken != token {
		e.RemoteToken = token
		changed = true
	}
	title := a.Name
	if title == "" {
		title = label
	}
	changed = setString(&e.A.ID, remotefeed.AgentID(slug, a.ID)) || changed
	changed = setString(&e.A.Title, title) || changed
	changed = setString(&e.A.Tool, slug) || changed
	changed = setString(&e.A.App, slug) || changed
	changed = setString(&e.A.ModelLabel, label) || changed
	changed = setString(&e.A.Folder, a.Group) || changed
	changed = setString(&e.A.Cwd, a.Group) || changed
	changed = setString(&e.A.LastPrompt, a.Summary) || changed
	e.A.External = true
	if !when.IsZero() && e.A.StatusSince.After(when) {
		// Add() stamps statusSince with the poll time. Show last_activity.
		e.A.StatusSince = when.UTC()
		changed = true
	}
	if !when.IsZero() && (e.A.StartedAt.IsZero() || e.A.StartedAt.After(when)) {
		e.A.StartedAt = when.UTC()
		changed = true
	}
	if next == model.Running {
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
		if e.A.Error == nil || e.A.Error.Message != msg {
			e.A.Error = &model.ErrInfo{Message: msg}
			changed = true
		}
	}
	if next == model.Waiting && (e.A.Waiting == nil || prevToken != token) {
		e.A.Waiting = &model.Prompt{
			ID: e.NextPromptID(), Kind: "input", Title: "needs you",
			Detail: a.Summary, Context: "open it in " + slug,
		}
		changed = true
	}
	if next == model.Idle && (prev == model.Running || prev == model.Waiting || prev == model.Starting) {
		e.A.Unseen = true
		changed = true
	}
	return changed
}

// feedToken is the hide key. A row that goes stale is a different token, so
// a hidden agent comes back when it stops being a live "working" report.
func feedToken(a remotefeed.Agent, docFetched, loaded, now time.Time, window time.Duration) string {
	token := a.Fingerprint()
	if a.Stale(docFetched, loaded, now, window) {
		token += "|stale"
	}
	return token
}

func (d *Daemon) feedSend(e store.Entry, _ string) error {
	return feedReadOnly(e)
}

func (d *Daemon) feedInterrupt(e store.Entry) error {
	return feedReadOnly(e)
}

func feedReadOnly(e store.Entry) error {
	name := e.A.App
	if name == "" {
		name = "the feed"
	}
	return errors.New("this agent is read-only on the deck — open it in " + name + " to answer or stop it")
}

// feedFocus opens the agent's open_url.
func (d *Daemon) feedFocus(e store.Entry) error {
	d.Store.Update(e.A.ID, func(x *store.Entry) bool {
		was := x.A.Unseen
		x.A.Unseen = false
		return was
	})
	open := d.feed.open
	if open == nil {
		open = openPage
	}
	if err := open(e.OpenURL); err != nil {
		return err
	}
	log.Printf("focus %s (%s): opened its page", e.A.ID, e.A.App)
	return nil
}

// feedHide keeps a feed agent off the board until its row changes.
func (d *Daemon) feedHide(e store.Entry) {
	d.feed.mu.Lock()
	if d.feed.hidden == nil {
		d.feed.hidden = map[string]string{}
	}
	d.feed.hidden[e.A.ID] = e.RemoteToken
	d.feed.mu.Unlock()
	d.Store.Remove(e.A.ID)
}
