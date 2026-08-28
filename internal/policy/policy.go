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
//
// AutoUpdate/PinnedCliVersion gate the CLI self-updater (internal/selfupdate).
// AutoUpdate is a POINTER so an absent field ("unknown") is distinguishable
// from an explicit false — unknown must fail OPEN (auto-update stays on), the
// deliberate OPPOSITE of the fail-closed CaptureAssistantProse rule, because a
// network/parse blip must never STRAND the fleet on an old binary.
// Ingest is the backend's advertised ingest capabilities — the `ingest` block on
// GET /v1/teams/policy. Absent on any backend older than the batch route, which
// is exactly why every field is optional and the zero value means "send events
// one at a time".
//
// FAIL-CLOSED, and in the useful direction: per-event delivery works against
// every backend that has ever existed, batch delivery works only against one
// that advertises it. Guessing wrong toward per-event costs throughput; guessing
// wrong toward batch would POST to a route that isn't there. The drain still
// carries its own runtime fallback (ingest.ErrBatchUnsupported) because an
// advertisement can go stale — a rollback between the fetch and the send — but
// that fallback is the second line, not the first.
type Ingest struct {
	// Batch reports whether POST <BatchEndpoint> exists.
	Batch bool `json:"batch"`
	// BatchEndpoint is the route, sent by the backend rather than hardcoded here
	// so the path can move without stranding the fleet on a 404.
	BatchEndpoint string `json:"batchEndpoint"`
	// MaxBatchSize is the largest member count the backend will accept.
	MaxBatchSize int `json:"maxBatchSize"`
}

type apiResponse struct {
	CaptureAssistantProse bool   `json:"captureAssistantProse"`
	CursorVendorUsage     bool   `json:"cursorVendorUsage"`
	AutoUpdate            *bool  `json:"autoUpdate"`
	PinnedCliVersion      string `json:"pinnedCliVersion"`
	MinCliVersion         string `json:"minCliVersion"`
	// Ingest is a POINTER so an absent block ("this backend predates batch") is
	// distinguishable from one that explicitly advertises batch:false. Both
	// resolve to per-event delivery today, but only one of them is a backend
	// that could ever say yes, and a future probe may want to tell them apart.
	Ingest *Ingest `json:"ingest"`
}

// diskCache is the on-disk shape: the resolved flags plus when they were
// fetched. AutoUpdate is a pointer for the same unknown-vs-false reason as
// apiResponse.
type diskCache struct {
	CaptureAssistantProse bool      `json:"captureAssistantProse"`
	CursorVendorUsage     bool      `json:"cursorVendorUsage"`
	AutoUpdate            *bool     `json:"autoUpdate"`
	PinnedCliVersion      string    `json:"pinnedCliVersion"`
	MinCliVersion         string    `json:"minCliVersion"`
	Ingest                *Ingest   `json:"ingest"`
	FetchedAt             time.Time `json:"fetchedAt"`
}

// Resolver caches the org's capture policy for one CLI process. Safe for
// concurrent use (the watch loops read it from their poll goroutines while a
// refresh may be in flight).
type Resolver struct {
	apiKey string

	mu                sync.Mutex
	value             bool
	cursorVendorUsage bool
	fetchedAt         time.Time // time of the last SUCCESSFUL fetch (zero = never)

	// autoUpdate is the org's self-update switch. It defaults to true and is
	// only flipped off by an explicit `autoUpdate:false` from the backend, so an
	// unknown/never-fetched policy leaves auto-update ON (fail-OPEN).
	autoUpdate bool
	// pinnedCliVersion, when non-empty, pins the fleet to an exact CLI tag.
	pinnedCliVersion string
	// minCliVersion, when non-empty, is the floor the org wants the fleet on.
	// It does not change WHAT gets installed — only how soon the updater looks
	// (see selfupdate.runAutoUpdate).
	minCliVersion string

	// ingest is the backend's advertised ingest capability. Nil until a fetch (or
	// an adopted disk cache) supplies one, and nil resolves to per-event.
	ingest *Ingest
}

// NewResolver builds a Resolver for the given PSE engineer key. If a recent
// (within cacheTTL) successful fetch was persisted by an earlier run, the prose
// flag is adopted so the process starts from the last known-good policy rather
// than false — the "stale cache within the refresh window may be used"
// allowance. The auto-update fields are adopted whenever a cache exists (no TTL
// gate): unlike prose, self-update tolerates a stale value, so the last known
// pin/switch is preferred over resetting to the default.
func NewResolver(apiKey string) *Resolver {
	r := &Resolver{apiKey: apiKey, autoUpdate: true}
	if c, ok := readDiskCache(); ok {
		if time.Since(c.FetchedAt) < cacheTTL {
			r.value = c.CaptureAssistantProse
			r.cursorVendorUsage = c.CursorVendorUsage
			r.fetchedAt = c.FetchedAt
			// TTL-gated like prose, not TTL-free like the self-update fields. An
			// aged-out capability resolves to per-event, which every backend
			// accepts, so the conservative branch costs throughput and nothing
			// else. The self-update fields cannot do that — reverting them to a
			// default would strand the fleet — which is why they age differently.
			r.ingest = c.Ingest
		}
		if c.AutoUpdate != nil {
			r.autoUpdate = *c.AutoUpdate
		}
		r.pinnedCliVersion = c.PinnedCliVersion
		r.minCliVersion = c.MinCliVersion
	}
	return r
}

// CursorVendorUsage reports whether the account-credential collector is
// affirmatively permitted by a fresh policy response. Like assistant prose it
// fails closed on startup, parse/network failure, and cache expiry.
func (r *Resolver) CursorVendorUsage() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cursorVendorUsage && !r.fetchedAt.IsZero() && time.Since(r.fetchedAt) < cacheTTL
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

// AutoUpdateEnabled reports whether the org allows CLI self-update. It is
// fail-OPEN: true unless a successful fetch (or adopted disk cache) explicitly
// set autoUpdate:false. A never-fetched or failing resolver therefore leaves
// auto-update ON — the deliberate opposite of CaptureAssistantProse, so a
// network blip cannot strand the fleet on an old binary. No TTL decay: a stale
// value keeps being used until the next successful refresh replaces it.
func (r *Resolver) AutoUpdateEnabled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.autoUpdate
}

// PinnedCliVersion returns the exact CLI tag the org pins the fleet to, or ""
// when unpinned. Like AutoUpdateEnabled it uses the last known value with no TTL
// decay.
func (r *Resolver) PinnedCliVersion() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pinnedCliVersion
}

// MinCliVersion returns the version floor the org wants the fleet on, or "".
// Like the other self-update fields it uses the last known value with no TTL
// decay. It is an ESCALATION lever only: it makes a lagging watcher check
// sooner than the normal cadence, and never overrides the autoUpdate switch or a
// pin — both of those are still enforced in selfupdate.checkAndApply.
func (r *Resolver) MinCliVersion() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.minCliVersion
}

// BatchIngest reports the backend's batch-ingest capability, WITHOUT any network
// call — the last fetched (or adopted) value.
//
// Returns endpoint and maxSize only when all three of batch, a non-empty
// endpoint, and a positive size are present. A half-advertised capability
// resolves to "unsupported": there is no useful way to batch against a backend
// that says yes but will not say where or how many, and inventing a default for
// the missing half is how a client ends up POSTing to a route that isn't there.
func (r *Resolver) BatchIngest() (endpoint string, maxSize int, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ingest == nil || !r.ingest.Batch {
		return "", 0, false
	}
	if r.ingest.BatchEndpoint == "" || r.ingest.MaxBatchSize <= 0 {
		return "", 0, false
	}
	return r.ingest.BatchEndpoint, r.ingest.MaxBatchSize, true
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
		r.StartBackgroundAfterRefresh(ctx)
	}()
}

// StartBackgroundAfterRefresh starts only the recurring portion of policy
// refresh. It is for a caller that must await the first policy decision before
// starting a sensitive collector, avoiding two simultaneous initial requests.
func (r *Resolver) StartBackgroundAfterRefresh(ctx context.Context) {
	go func() {
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
	parsed, err := fetchPolicy(r.apiKey)
	if err != nil {
		// Fail-closed for prose: keep the last good value (if any);
		// CaptureAssistantProse enforces cacheTTL so a stale value can't be
		// trusted indefinitely. Fail-OPEN for auto-update: the existing
		// autoUpdate/pin fields are left untouched, so a failed refresh never
		// flips self-update off.
		return
	}
	now := time.Now()
	// Auto-update defaults ON when the field is absent (unknown != disabled).
	autoUpdate := true
	if parsed.AutoUpdate != nil {
		autoUpdate = *parsed.AutoUpdate
	}
	r.mu.Lock()
	r.value = parsed.CaptureAssistantProse
	r.cursorVendorUsage = parsed.CursorVendorUsage
	r.fetchedAt = now
	r.autoUpdate = autoUpdate
	r.pinnedCliVersion = parsed.PinnedCliVersion
	r.minCliVersion = parsed.MinCliVersion
	// Assigned unconditionally, INCLUDING to nil. A backend that stops
	// advertising batch (a rollback) must retract the capability here rather
	// than leave the last good one in place — the opposite of the self-update
	// fields' retain-on-anything rule, because the safe direction is reversed.
	r.ingest = parsed.Ingest
	r.mu.Unlock()
	writeDiskCache(diskCache{
		CaptureAssistantProse: parsed.CaptureAssistantProse,
		CursorVendorUsage:     parsed.CursorVendorUsage,
		AutoUpdate:            parsed.AutoUpdate,
		PinnedCliVersion:      parsed.PinnedCliVersion,
		MinCliVersion:         parsed.MinCliVersion,
		Ingest:                parsed.Ingest,
		FetchedAt:             now,
	})
}

// fetchPolicy does the GET and parses the response. Returns an error on any
// non-200, transport failure, or unparseable body so the caller fails closed
// (prose) / retains its prior value (auto-update).
func fetchPolicy(apiKey string) (apiResponse, error) {
	req, err := http.NewRequest(http.MethodGet, ingest.APIURL()+policyPath(), nil)
	if err != nil {
		return apiResponse{}, err
	}
	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := ingest.HTTPClient().Do(req)
	if err != nil {
		return apiResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiResponse{}, &httpError{status: resp.StatusCode}
	}
	var parsed apiResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxPolicyBodyBytes)).Decode(&parsed); err != nil {
		return apiResponse{}, err
	}
	return parsed, nil
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
