// Package policy resolves and caches the org-level teams capture policy that
// gates opt-in behaviors on-device. Today it carries a single flag —
// captureAssistantProse — fetched from GET /v1/teams/policy and threaded into
// the projection choke point (redact.ProjectEvent) by the watch loops.
//
// The design is FAIL-CLOSED by construction: the resolved value is false unless
// a SUCCESSFUL, recent fetch affirmatively set it true. Every failure path
// (network error, non-200, unparseable body, teams-not-configured 503, missing
// cache) resolves to false, so assistant prose is never captured on doubt.
package policy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// maxPolicyBodyBytes caps how much of the policy response we read. The payload
// is a single boolean object (tens of bytes); 64 KB is generous slack while
// bounding memory against a malformed or hostile response.
const maxPolicyBodyBytes = 64 << 10

const (
	// RefreshInterval is how often a running watcher re-fetches the policy from
	// the backend. The watch loop calls Refresh once at start and then on this
	// cadence.
	RefreshInterval = 10 * time.Minute

	// cacheTTL is how long a successfully-fetched value stays trusted without a
	// fresh confirmation. It is longer than RefreshInterval so a single failed
	// refresh doesn't flip capture off mid-window; a sustained outage (two-plus
	// consecutive failures) ages the value out and CaptureAssistantProse fails
	// closed to false.
	cacheTTL = 15 * time.Minute

	// cacheFileName is the on-disk cache under the state dir. It lets a fresh
	// process reuse a recent successful fetch (within cacheTTL) instead of
	// starting from false until its first refresh completes.
	cacheFileName = "teams-policy.json"

	// defaultPolicyPath is the backend route. Override with
	// PROMPTSTER_TEAMS_POLICY_PATH (parity with PROMPTSTER_TEAMS_INGEST_PATH).
	defaultPolicyPath = "/v1/teams/policy"
)

// policyPath returns the policy route, honoring the env override.
func policyPath() string {
	if p := os.Getenv("PROMPTSTER_TEAMS_POLICY_PATH"); p != "" {
		return p
	}
	return defaultPolicyPath
}

// apiResponse mirrors GET /v1/teams/policy.
type apiResponse struct {
	CaptureAssistantProse bool `json:"captureAssistantProse"`
}

// diskCache is the on-disk shape: the resolved flag plus when it was fetched.
type diskCache struct {
	CaptureAssistantProse bool      `json:"captureAssistantProse"`
	FetchedAt             time.Time `json:"fetchedAt"`
}

// Resolver caches the org's capture policy for one CLI process. Safe for
// concurrent use (the watch loops read it from their poll goroutines while a
// refresh may be in flight).
type Resolver struct {
	apiKey string

	mu        sync.Mutex
	value     bool
	fetchedAt time.Time // time of the last SUCCESSFUL fetch (zero = never)
}

// NewResolver builds a Resolver for the given PSE engineer key. If a recent
// (within cacheTTL) successful fetch was persisted by an earlier run, it is
// adopted so the process starts from the last known-good policy rather than
// false — the "stale cache within the refresh window may be used" allowance.
func NewResolver(apiKey string) *Resolver {
	r := &Resolver{apiKey: apiKey}
	if c, ok := readDiskCache(); ok && time.Since(c.FetchedAt) < cacheTTL {
		r.value = c.CaptureAssistantProse
		r.fetchedAt = c.FetchedAt
	}
	return r
}

// CaptureAssistantProse returns the current policy WITHOUT any network call:
// the cached value from the last successful fetch, but only while it is still
// within cacheTTL. It is false until a successful Refresh (or an adopted disk
// cache) sets it true, and it decays back to false once the last good fetch
// ages out — so a watcher whose refreshes all fail fails closed on its own.
func (r *Resolver) CaptureAssistantProse() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.value && !r.fetchedAt.IsZero() && time.Since(r.fetchedAt) < cacheTTL {
		return true
	}
	return false
}

// StartBackground runs policy refreshes OFF the caller's hot path: it fires an
// immediate refresh, then one every RefreshInterval, in a goroutine that exits
// when ctx is cancelled. The capture loop only ever reads CaptureAssistantProse
// (lock-guarded, no network), so it never blocks on the 15s-timeout policy
// fetch — the reason this replaced the inline Refresh in the watch loops.
// Fail-closed is unchanged: prose stays off until the first successful fetch
// completes.
func (r *Resolver) StartBackground(ctx context.Context) {
	go func() {
		r.Refresh()
		ticker := time.NewTicker(RefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.Refresh()
			}
		}
	}()
}

// Refresh performs one network fetch and, on success, updates the cached value
// (memory + disk) and its freshness timestamp. On ANY error it is a no-op: the
// prior value is retained and left to age out via cacheTTL, so a transient blip
// doesn't immediately drop capture but a real outage still fails closed. Safe
// to call at watch start and on a ticker.
func (r *Resolver) Refresh() {
	val, err := fetchPolicy(r.apiKey)
	if err != nil {
		// Fail-closed: keep the last good value (if any); CaptureAssistantProse
		// enforces cacheTTL so a stale value can't be trusted indefinitely.
		return
	}
	now := time.Now()
	r.mu.Lock()
	r.value = val
	r.fetchedAt = now
	r.mu.Unlock()
	writeDiskCache(diskCache{CaptureAssistantProse: val, FetchedAt: now})
}

// fetchPolicy does the GET and parses the response. Returns an error on any
// non-200, transport failure, or unparseable body so the caller fails closed.
func fetchPolicy(apiKey string) (bool, error) {
	req, err := http.NewRequest(http.MethodGet, ingest.APIURL()+policyPath(), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := ingest.HTTPClient().Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, &httpError{status: resp.StatusCode}
	}
	var parsed apiResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxPolicyBodyBytes)).Decode(&parsed); err != nil {
		return false, err
	}
	return parsed.CaptureAssistantProse, nil
}

// httpError is a non-200 policy response.
type httpError struct{ status int }

func (e *httpError) Error() string {
	return "policy fetch: unexpected HTTP status " + http.StatusText(e.status)
}

func cacheFilePath() string {
	return filepath.Join(state.StateDir(), cacheFileName)
}

func readDiskCache() (diskCache, bool) {
	data, err := os.ReadFile(cacheFilePath())
	if err != nil {
		return diskCache{}, false
	}
	var c diskCache
	if err := json.Unmarshal(data, &c); err != nil {
		return diskCache{}, false
	}
	if c.FetchedAt.IsZero() {
		return diskCache{}, false
	}
	return c, true
}

func writeDiskCache(c diskCache) {
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	dir := state.StateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	// Per-process temp file in the SAME dir, then atomic rename onto the final
	// path. A shared literal ".tmp" path races when claude-watch + codex-watch
	// write concurrently (and os.Rename fails on Windows when the dest exists),
	// silently dropping the update; a unique temp per write avoids the collision.
	tmp, err := os.CreateTemp(dir, "teams-policy-*.json.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	// os.CreateTemp already made the file 0600, so no chmod is needed.
	if err := os.Rename(tmpName, cacheFilePath()); err != nil {
		_ = os.Remove(tmpName)
	}
}
