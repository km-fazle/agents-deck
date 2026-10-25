package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brushknight/agents-deck/backend/internal/model"
	"github.com/brushknight/agents-deck/backend/internal/remotefeed"
	"github.com/brushknight/agents-deck/backend/internal/store"
)

func TestRemoteFeedExampleOnTheDeck(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "fixtures", "remote-feed.json"))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := remotefeed.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{Store: store.New("", "test", "0"), Cfg: Config{RemoteFeed: &RemoteFeedConfig{StaleSeconds: 180}}}
	now := snap.FetchedAt.Add(30 * time.Second)
	label, slug := d.feedNames(snap)
	if label != "Grok" || slug != "grok" {
		t.Fatalf("names: %s %s", label, slug)
	}
	d.syncFeed(label, slug, snap, now, now)

	inbox, ok := d.Store.Get(remotefeed.AgentID("grok", "inbox"))
	if !ok || inbox.A.Status != model.Running || inbox.A.App != "grok" || inbox.A.ModelLabel != "Grok" ||
		inbox.A.Title != "Inbox triage" || inbox.A.Folder != "ops" || inbox.FeedSource != "grok" ||
		inbox.A.Activity == nil || inbox.A.Activity.Detail != "Reading the latest thread" ||
		inbox.OpenURL != "https://example.com/bots/inbox" {
		t.Fatalf("inbox: %+v activity %+v", inbox.A, inbox.A.Activity)
	}
	review, _ := d.Store.Get(remotefeed.AgentID("grok", "review"))
	if review.A.Status != model.Waiting || review.A.Waiting == nil || len(review.A.Waiting.Options) != 0 ||
		review.A.Waiting.Context != "open it in grok" || review.A.Waiting.Detail != "Which draft should go out?" {
		t.Fatalf("review: %+v prompt %+v", review.A, review.A.Waiting)
	}
	digest, _ := d.Store.Get(remotefeed.AgentID("grok", "digest"))
	if digest.A.Status != model.Idle {
		t.Fatalf("digest: %s", digest.A.Status)
	}
	pager, _ := d.Store.Get(remotefeed.AgentID("grok", "pager"))
	if pager.A.Status != model.Error || pager.A.Error == nil || pager.A.Error.Message != "The mailbox refused the login" {
		t.Fatalf("pager: %+v", pager.A)
	}
	archive, _ := d.Store.Get(remotefeed.AgentID("grok", "archive"))
	if archive.A.Status != model.Error || archive.A.Error == nil || archive.A.Error.Message != "stale" || archive.A.Activity != nil {
		t.Fatalf("stale working agent: %+v activity %+v", archive.A, archive.A.Activity)
	}

	// The same feed, read long enough ago that every tile is stale.
	d.syncFeed(label, slug, snap, now, now.Add(10*time.Minute))
	inbox, _ = d.Store.Get(remotefeed.AgentID("grok", "inbox"))
	if inbox.A.Status != model.Error || inbox.A.Error == nil || inbox.A.Error.Message != "stale" {
		t.Fatalf("old fetch still looks live: %+v", inbox.A)
	}

	// A fresh read brings the working agent back. Focus opens its page.
	// Interrupt is refused.
	d.syncFeed(label, slug, snap, now, now)
	inbox, _ = d.Store.Get(remotefeed.AgentID("grok", "inbox"))
	if inbox.A.Status != model.Running {
		t.Fatalf("fresh feed: %s", inbox.A.Status)
	}
	if err := d.Interrupt(inbox.A.ID); err == nil || !contains(err.Error(), "read-only") {
		t.Fatalf("interrupt: %v", err)
	}
	var opened string
	d.feed.open = func(u string) error { opened = u; return nil }
	if err := d.Focus(inbox.A.ID); err != nil || opened != inbox.OpenURL {
		t.Fatalf("focus: %v %q", err, opened)
	}

	d.Dismiss(digest.A.ID)
	d.syncFeed(label, slug, snap, now, now)
	if _, ok := d.Store.Get(digest.A.ID); ok {
		t.Fatal("hidden idle agent came back unchanged")
	}
	// An agent that leaves the file leaves the board.
	snap.Agents = snap.Agents[:1]
	d.syncFeed(label, slug, snap, now, now)
	if _, ok := d.Store.Get(remotefeed.AgentID("grok", "pager")); ok {
		t.Fatal("removed feed row stayed on the board")
	}
	if _, ok := d.Store.Get(remotefeed.AgentID("grok", "inbox")); !ok {
		t.Fatal("inbox was removed with the other rows")
	}
}

func TestFeedTokenAndDisabled(t *testing.T) {
	d := &Daemon{}
	if d.feedOn() {
		t.Fatal("nil feed is on")
	}
	off := false
	d.Cfg.RemoteFeed = &RemoteFeedConfig{Enabled: &off, Path: "feed.json"}
	if d.feedOn() {
		t.Fatal("disabled feed is on")
	}
	d.Cfg.RemoteFeed = &RemoteFeedConfig{Path: "feed.json", Name: "Grok"}
	if !d.feedOn() || d.feedPoll() != 20*time.Second || d.feedStaleWindow() != 3*time.Minute {
		t.Fatalf("defaults poll %s stale %s", d.feedPoll(), d.feedStaleWindow())
	}
	t.Setenv("REMOTE_FEED_TOKEN_TEST", "from-env")
	d.Cfg.RemoteFeed.Token = "from-file"
	d.Cfg.RemoteFeed.TokenEnv = "REMOTE_FEED_TOKEN_TEST"
	if d.Cfg.RemoteFeed.bearer() != "from-env" {
		t.Fatal(d.Cfg.RemoteFeed.bearer())
	}
}
