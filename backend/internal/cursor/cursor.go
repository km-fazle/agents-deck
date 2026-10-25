// Package cursor reads Cursor cloud agents from the Cloud Agents API.
//
// The current API (v1) splits a durable agent from its runs, and a list item
// has no repository, branch, pull request or summary. Those take a further
// request per agent, which a board polling every few seconds would spend
// against the API's rate limit. The legacy list, GET /v0/agents, is still
// documented and returns all of that in one response, so the deck uses it.
//
// The API key is sent only as Basic auth to api.cursor.com. It is never
// written into an error, a log line or a URL.
package cursor

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
	"strings"
	"time"

	"github.com/brushknight/agents-deck/backend/internal/model"
)

const (
	// DefaultBase is the Cloud Agents API.
	DefaultBase = "https://api.cursor.com"
	// PageLimit is the most agents one list call returns (the API's max).
	PageLimit = 100
	// MaxPages caps a single poll so a long history can't turn into a burst.
	MaxPages = 3
)

// Client lists cloud agents.
type Client struct {
	BaseURL string       // empty: DefaultBase
	Key     string       // API key; Basic auth, empty password
	HTTP    *http.Client // nil: a client that does not follow redirects
}

// Agent is one cloud agent, flattened from a /v0/agents list item.
type Agent struct {
	ID         string
	Name       string
	Status     string
	Repository string
	Ref        string
	Branch     string
	URL        string // the agent's page on cursor.com
	PRURL      string
	Summary    string
	Created    time.Time
	Updated    time.Time
}

// APIError is a non-2xx response. Error() never includes the response body
// or the API key. RetryAfter is the Retry-After header, when the server sent one.
type APIError struct {
	Status     int
	RetryAfter string
}

func (e *APIError) Error() string {
	if e == nil {
		return "cursor api: error"
	}
	return fmt.Sprintf("cursor api: %d", e.Status)
}

// HTTPClient is the client the watcher uses: a short timeout, and no
// redirects, so the API key stays on the host it was sent to.
func HTTPClient() *http.Client {
	return &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Agents lists cloud agents, following the page cursor for up to MaxPages.
func (c Client) Agents(ctx context.Context) ([]Agent, error) {
	if strings.TrimSpace(c.Key) == "" {
		return nil, errors.New("cursor: no API key")
	}
	var all []Agent
	pageCursor := ""
	for page := 0; page < MaxPages; page++ {
		batch, next, err := c.page(ctx, pageCursor)
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if next == "" || next == pageCursor {
			break
		}
		pageCursor = next
	}
	return all, nil
}

func (c Client) base() string {
	b := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if b == "" {
		b = DefaultBase
	}
	return b
}

// CheckBase accepts the Cloud Agents host, and 127.0.0.1 or ::1 so tests
// can stand in for it. The API key is never sent anywhere else.
func CheckBase(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = DefaultBase
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil {
		return errors.New("cursor api: base url is not usable")
	}
	host := u.Hostname()
	if host == "127.0.0.1" || host == "::1" {
		if u.Scheme == "http" || u.Scheme == "https" {
			return nil
		}
	}
	if u.Scheme == "https" && host == "api.cursor.com" {
		return nil
	}
	return errors.New("cursor api: base url must be https://api.cursor.com")
}

func (c Client) page(ctx context.Context, pageCursor string) ([]Agent, string, error) {
	if err := CheckBase(c.base()); err != nil {
		return nil, "", err
	}
	q := url.Values{}
	q.Set("limit", fmt.Sprint(PageLimit))
	if pageCursor != "" {
		q.Set("cursor", pageCursor)
	}
	endpoint := c.base() + "/v0/agents?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", err
	}
	req.SetBasicAuth(c.Key, "")
	req.Header.Set("Accept", "application/json")
	ht := c.HTTP
	if ht == nil {
		ht = HTTPClient()
	}
	resp, err := ht.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("cursor api: %v", redact(err.Error(), c.Key))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, "", fmt.Errorf("cursor api: %v", redact(err.Error(), c.Key))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", &APIError{Status: resp.StatusCode, RetryAfter: resp.Header.Get("Retry-After")}
	}
	return ParseList(body)
}

type rawAgent struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	Summary   string `json:"summary"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	Source    struct {
		Repository string `json:"repository"`
		Ref        string `json:"ref"`
	} `json:"source"`
	Target struct {
		BranchName string `json:"branchName"`
		URL        string `json:"url"`
		PRURL      string `json:"prUrl"`
	} `json:"target"`
}

type rawPage struct {
	Agents     *[]rawAgent `json:"agents"`
	NextCursor string      `json:"nextCursor"`
}

// ParseList decodes one /v0/agents response body.
func ParseList(body []byte) ([]Agent, string, error) {
	var page rawPage
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, "", errors.New("cursor api: response is not a list of agents")
	}
	if page.Agents == nil {
		if bytesHas(body, `"items"`) {
			return nil, "", errors.New("cursor api: response has items but not agents; the deck polls /v0/agents")
		}
		return nil, "", errors.New("cursor api: response has no agents")
	}
	out := make([]Agent, 0, len(*page.Agents))
	for _, r := range *page.Agents {
		if strings.TrimSpace(r.ID) == "" {
			continue
		}
		out = append(out, Agent{
			ID:         r.ID,
			Name:       strings.TrimSpace(r.Name),
			Status:     strings.TrimSpace(r.Status),
			Repository: stripUserinfo(r.Source.Repository),
			Ref:        strings.TrimSpace(r.Source.Ref),
			Branch:     strings.TrimSpace(r.Target.BranchName),
			URL:        strings.TrimSpace(r.Target.URL),
			PRURL:      strings.TrimSpace(r.Target.PRURL),
			Summary:    strings.TrimSpace(r.Summary),
			Created:    parseTime(r.CreatedAt),
			Updated:    parseTime(r.UpdatedAt),
		})
	}
	return out, page.NextCursor, nil
}

func bytesHas(b []byte, s string) bool {
	return strings.Contains(string(b), s)
}

// MapStatus turns a Cloud Agents status into a deck status.
//
// Documented on GET /v0/agents: CREATING, RUNNING, FINISHED. The current
// run API also names ERROR, CANCELLED and EXPIRED, and the v1 agent
// lifecycle names ACTIVE, IDLE and ARCHIVED. Anything else that asks for a
// person (the word does not appear in the v0 examples) becomes waiting.
// An unknown word becomes idle, so a new status can't show up as working.
func MapStatus(status string) model.Status {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "CREATING", "STARTING":
		return model.Starting
	case "RUNNING", "ACTIVE":
		return model.Running
	case "ERROR", "FAILED", "FAILURE":
		return model.Error
	case "FINISHED", "IDLE", "DONE", "COMPLETED", "COMPLETE",
		"CANCELLED", "CANCELED", "STOPPED", "EXPIRED", "ARCHIVED":
		return model.Idle
	default:
		u := strings.ToUpper(status)
		switch {
		case strings.Contains(u, "WAIT"), strings.Contains(u, "INPUT"),
			strings.Contains(u, "NEED"), strings.Contains(u, "BLOCK"),
			strings.Contains(u, "ATTENTION"):
			return model.Waiting
		case strings.Contains(u, "ERR"), strings.Contains(u, "FAIL"):
			return model.Error
		default:
			return model.Idle
		}
	}
}

// Live is a status the board keeps even when the agent is older than the
// window: still starting, still working, or waiting on a person.
func Live(status string) bool {
	switch MapStatus(status) {
	case model.Running, model.Starting, model.Waiting:
		return true
	default:
		return false
	}
}

// When is the agent's last activity: updatedAt when the API sends it,
// otherwise createdAt.
func (a Agent) When() time.Time {
	if !a.Updated.IsZero() {
		return a.Updated
	}
	return a.Created
}

// RepoLabel is "org/repo" with the scheme, any userinfo and a trailing
// ".git" removed. Userinfo is dropped so a credential in a repository URL
// never reaches the board.
func (a Agent) RepoLabel() string {
	return repoLabel(a.Repository)
}

func repoLabel(raw string) string {
	raw = stripUserinfo(strings.TrimSpace(raw))
	raw = strings.TrimSuffix(raw, ".git")
	raw = strings.TrimSuffix(raw, "/")
	if i := strings.Index(raw, "://"); i >= 0 {
		raw = raw[i+3:]
	}
	if i := strings.Index(raw, "/"); i >= 0 {
		host := raw[:i]
		if strings.Contains(host, ".") {
			raw = raw[i+1:]
		}
	}
	return strings.Trim(raw, "/")
}

// stripUserinfo removes user:password@ from a URL or a scp-like host.
func stripUserinfo(raw string) string {
	raw = strings.TrimSpace(raw)
	if u, err := url.Parse(raw); err == nil && u.Host != "" && u.User != nil {
		u.User = nil
		return u.String()
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if at := strings.Index(rest, "@"); at >= 0 && !strings.Contains(rest[:at], "/") {
			return raw[:i+3] + rest[at+1:]
		}
	}
	return raw
}

// BranchName is the branch the agent is pushing, or the ref it started from.
func (a Agent) BranchName() string {
	if a.Branch != "" {
		return a.Branch
	}
	return a.Ref
}

// PageURL is the cursor.com page for this agent. The list's target.url wins;
// otherwise the v0 page the docs show.
func (a Agent) PageURL() string {
	if a.URL != "" {
		return a.URL
	}
	if a.ID == "" {
		return ""
	}
	return "https://cursor.com/agents?id=" + url.QueryEscape(a.ID)
}

// Detail is the short line for a working tile: repo and branch, or the summary.
func (a Agent) Detail() string {
	repo, branch := a.RepoLabel(), a.BranchName()
	switch {
	case repo != "" && branch != "":
		return repo + " · " + branch
	case repo != "":
		return repo
	default:
		return a.Summary
	}
}

// Note is the card's last-prompt text: the summary, then the pull request
// link when there is one.
func (a Agent) Note() string {
	switch {
	case a.Summary != "" && a.PRURL != "":
		return a.Summary + "\n" + a.PRURL
	case a.Summary != "":
		return a.Summary
	default:
		return a.PRURL
	}
}

// ErrorText is the line on an error tile.
func (a Agent) ErrorText() string {
	if a.Summary != "" {
		return a.Summary
	}
	return "cloud agent error"
}

// Include reports whether the agent belongs on the board. Live agents always
// do. Finished, failed and archived ones do only when their last activity
// falls inside window. A zero window means no age limit.
func (a Agent) Include(now time.Time, window time.Duration) bool {
	if Live(a.Status) {
		return true
	}
	if window == 0 {
		return true
	}
	when := a.When()
	if when.IsZero() {
		return false
	}
	return !now.Before(when) && now.Sub(when) <= window
}

// AgentID is the deck id for a cloud agent: "c" plus the start of a hash of
// its API id. Deck Link carries ids in 12 bytes.
func AgentID(remoteID string) string {
	sum := sha256.Sum256([]byte("cursor:" + remoteID))
	return "c" + hex.EncodeToString(sum[:])[:5]
}

// Fingerprint changes when the agent is worth showing again after a hide.
func (a Agent) Fingerprint() string {
	when := ""
	if t := a.When(); !t.IsZero() {
		when = t.UTC().Format(time.RFC3339)
	}
	return a.Status + "|" + when + "|" + a.Summary + "|" + a.PRURL
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

func redact(msg, secret string) string {
	if secret == "" {
		return msg
	}
	return strings.ReplaceAll(msg, secret, "[redacted]")
}

// Backoff is how long to wait after a failed poll. 429 and 5xx back off,
// doubling from base up to 5 minutes, and a Retry-After on 429 wins.
// Anything else (a bad key, a 404) keeps the normal interval.
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

// retryAfter parses a Retry-After delta in seconds. An HTTP-date is ignored
// (the next poll still backs off by doubling).
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
