// Package remotefeed reads a status feed for agents that have no public API.
//
// A feed is a JSON document, either a local file or an https URL (with an
// optional bearer token). Grok bots are published this way: something else
// writes the file, and the deck only polls it. The schema is docs/remote-feed.md.
package remotefeed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/brushknight/agents-deck/backend/internal/model"
)

// Snapshot is one read of a feed.
type Snapshot struct {
	Source    string // the document's source label, when it has one
	FetchedAt time.Time
	Agents    []Agent
}

// Agent is one entry in the feed.
type Agent struct {
	ID           string
	Name         string
	Group        string
	Status       string
	Summary      string
	LastActivity time.Time
	OpenURL      string
}

// Source reads a feed from a file or an https URL.
type Source struct {
	URL   string // used when set; otherwise Path
	Path  string
	Token string // bearer token for URL; never logged
	HTTP  *http.Client
}

// HTTPClient does not follow redirects, so a bearer token stays on the host
// it was sent to.
func HTTPClient() *http.Client {
	return &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Load reads the feed once.
func (s Source) Load(ctx context.Context) (Snapshot, error) {
	if strings.TrimSpace(s.URL) != "" {
		return s.loadURL(ctx)
	}
	if strings.TrimSpace(s.Path) == "" {
		return Snapshot{}, errors.New("remote feed: no url or path")
	}
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("remote feed: %v", err)
	}
	return Parse(b)
}

func (s Source) loadURL(ctx context.Context) (Snapshot, error) {
	if err := CheckURL(s.URL); err != nil {
		return Snapshot{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return Snapshot{}, err
	}
	req.Header.Set("Accept", "application/json")
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	ht := s.HTTP
	if ht == nil {
		ht = HTTPClient()
	}
	resp, err := ht.Do(req)
	if err != nil {
		return Snapshot{}, fmt.Errorf("remote feed: %v", redact(err.Error(), s.Token))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return Snapshot{}, fmt.Errorf("remote feed: %v", redact(err.Error(), s.Token))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Snapshot{}, &APIError{Status: resp.StatusCode, RetryAfter: resp.Header.Get("Retry-After")}
	}
	snap, err := Parse(body)
	if err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

// APIError is a non-2xx response from a feed URL. Error() has no body, so a
// token echoed by the server is not returned.
type APIError struct {
	Status     int
	RetryAfter string
}

func (e *APIError) Error() string {
	if e == nil {
		return "remote feed: error"
	}
	return fmt.Sprintf("remote feed: %d", e.Status)
}

// CheckURL accepts https, and http only for 127.0.0.1 (a test server).
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil {
		return errors.New("remote feed: url is not usable")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if u.Hostname() == "127.0.0.1" {
			return nil
		}
		return errors.New("remote feed: url must be https")
	default:
		return errors.New("remote feed: url must be https")
	}
}

type rawFeed struct {
	Source    string     `json:"source"`
	FetchedAt string     `json:"fetched_at"`
	Fetched2  string     `json:"fetchedAt"`
	Agents    []rawAgent `json:"agents"`
}

type rawAgent struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Group         string `json:"group"`
	Team          string `json:"team"`
	Status        string `json:"status"`
	Summary       string `json:"summary"`
	LastMessage   string `json:"last_message"`
	LastMessage2  string `json:"lastMessage"`
	LastActivity  string `json:"last_activity"`
	LastActivity2 string `json:"lastActivity"`
	OpenURL       string `json:"open_url"`
	OpenURL2      string `json:"openUrl"`
}

// Parse decodes a feed document.
func Parse(b []byte) (Snapshot, error) {
	var raw rawFeed
	if err := json.Unmarshal(b, &raw); err != nil {
		return Snapshot{}, errors.New("remote feed: not a feed document")
	}
	if raw.Agents == nil {
		return Snapshot{}, errors.New("remote feed: document has no agents")
	}
	snap := Snapshot{Source: strings.TrimSpace(raw.Source), FetchedAt: parseTime(first(raw.FetchedAt, raw.Fetched2))}
	for _, r := range raw.Agents {
		id := strings.TrimSpace(r.ID)
		if id == "" {
			continue
		}
		group := strings.TrimSpace(r.Group)
		if group == "" {
			group = strings.TrimSpace(r.Team)
		}
		snap.Agents = append(snap.Agents, Agent{
			ID:           id,
			Name:         strings.TrimSpace(r.Name),
			Group:        group,
			Status:       strings.TrimSpace(r.Status),
			Summary:      strings.TrimSpace(first(r.Summary, r.LastMessage, r.LastMessage2)),
			LastActivity: parseTime(first(r.LastActivity, r.LastActivity2)),
			OpenURL:      strings.TrimSpace(first(r.OpenURL, r.OpenURL2)),
		})
	}
	return snap, nil
}

func first(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// Map turns a feed status into a deck status and, for an error tile, the
// message. Unknown words become idle, so a typo can't show up as working.
func (a Agent) Map() (model.Status, string) {
	switch strings.ToLower(strings.TrimSpace(a.Status)) {
	case "working", "running":
		return model.Running, ""
	case "idle", "done", "finished":
		return model.Idle, ""
	case "needs_input", "needs-input", "needsinput", "waiting", "needs_attention", "needs-attention":
		return model.Waiting, ""
	case "error", "failed", "failure":
		if a.Summary != "" {
			return model.Error, a.Summary
		}
		return model.Error, "error"
	case "offline":
		return model.Error, "offline"
	case "stale":
		return model.Error, "stale"
	default:
		return model.Idle, ""
	}
}

// LooksLive is a status that claims the agent is in the middle of something
// (working, or waiting on a person). A stale timestamp must not keep it.
func (a Agent) LooksLive() bool {
	st, _ := a.Map()
	return st == model.Running || st == model.Waiting || st == model.Starting
}

// Stale reports that this entry should show as stale rather than as its
// published status.
//
// The feed itself is stale when its fetched_at, or the time the deck last
// read it (loaded), is older than window. A working or needs-input agent is
// also stale when its own last_activity is older than window, even inside a
// fresh feed — a stuck "working" is not working. Idle and error rows in a
// fresh feed keep their status. A zero window turns the check off.
func (a Agent) Stale(docFetched, loaded, now time.Time, window time.Duration) bool {
	if window <= 0 {
		return false
	}
	if older(docFetched, now, window) || older(loaded, now, window) {
		return true
	}
	if a.LooksLive() && older(a.LastActivity, now, window) {
		return true
	}
	return false
}

func older(t, now time.Time, window time.Duration) bool {
	if t.IsZero() || now.Before(t) {
		return false
	}
	return now.Sub(t) > window
}

// When is last_activity, or the feed's fetched_at when the agent has none.
func (a Agent) When(docFetched time.Time) time.Time {
	if !a.LastActivity.IsZero() {
		return a.LastActivity
	}
	return docFetched
}

// Detail is the short line on a working tile.
func (a Agent) Detail() string {
	if a.Summary != "" {
		return a.Summary
	}
	return a.Group
}

// Fingerprint changes when a hidden agent should come back.
func (a Agent) Fingerprint() string {
	when := ""
	if !a.LastActivity.IsZero() {
		when = a.LastActivity.UTC().Format(time.RFC3339)
	}
	return a.Status + "|" + when + "|" + a.Summary
}

// Slug is the deck's app name for a source label: letters and digits, at
// most 11 characters (Deck Link's app field is 12 bytes). Names that already
// mean an interactive harness are rewritten so they don't gain answer buttons.
func Slug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if s == "" {
		s = "feed"
	}
	if len(s) > 11 {
		s = s[:11]
	}
	switch s {
	case "orca", "herdr", "cmux", "cursor":
		return "feed"
	default:
		return s
	}
}

// DisplayName is the label on the card. An empty config name means Grok.
func DisplayName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Grok"
	}
	return name
}

// AgentID is the deck id: "r" plus the start of a hash of the source and the
// feed id.
func AgentID(source, id string) string {
	sum := sha256.Sum256([]byte("feed:" + source + ":" + id))
	return "r" + hex.EncodeToString(sum[:])[:5]
}

func redact(msg, secret string) string {
	if secret == "" {
		return msg
	}
	return strings.ReplaceAll(msg, secret, "[redacted]")
}

// Backoff matches the Cursor poll: 429 and 5xx double the wait, up to 5
// minutes, and Retry-After on 429 wins. Other errors keep the normal interval.
func Backoff(current, base time.Duration, err error) time.Duration {
	var api *APIError
	if !errors.As(err, &api) || (api.Status != http.StatusTooManyRequests && api.Status < 500) {
		return base
	}
	if api.Status == http.StatusTooManyRequests {
		if d := retryAfter(api.RetryAfter); d > 0 {
			return capWait(d)
		}
	}
	n := current * 2
	if n < base*2 {
		n = base * 2
	}
	return capWait(n)
}

func capWait(d time.Duration) time.Duration {
	if d > 5*time.Minute {
		return 5 * time.Minute
	}
	if d < time.Second {
		return time.Second
	}
	return d
}

func retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
		if n > 3600 {
			return time.Hour
		}
	}
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}
