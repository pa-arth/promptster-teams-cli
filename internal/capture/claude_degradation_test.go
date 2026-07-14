package capture

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
)

// TestDegradationCountsParsesNotSends is the regression for bug 4.
//
// claudeDegradationStep's parameter is named `parsed` and its threshold means
// "the transcript format changed under us — the PARSER is broken". But the poll
// loop used to hand it a SEND count (incremented only on a 2xx), so a pure
// NETWORK failure — the 429 storm, an offline laptop, a backend outage — looked
// identical to a broken parser. That produced the observed
// "degraded — 271744 bytes consumed", which then handed capture to hooks. Hooks
// only cover the live tail, never a replay, so the outage window died twice:
// once to the failed POST, once to the dryRun discard.
//
// With sending moved to the outbox, the poll loop returns a true parse count, so
// a total send outage must NOT trip degraded.
func TestDegradationCountsParsesNotSends(t *testing.T) {
	root := claudeProjectsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))

	// Every request fails: a total, sustained send outage.
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	workspace := t.TempDir()
	dir := filepath.Join(root, "-workspace-slug")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	// A transcript with enough real prompts to blow past the degraded byte
	// threshold if (and only if) nothing counts as parsed.
	transcript := filepath.Join(dir, "sess-degrade.jsonl")
	f, err := os.Create(transcript) // #nosec G304 -- test temp path
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < 40; i++ {
		line := fmt.Sprintf(`{"type":"user","cwd":%q,"timestamp":%q,"message":{"role":"user","content":"prompt number %d"}}`+"\n",
			workspace, ts, i)
		if _, err := f.WriteString(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	session := Session{
		DeviceID: "sess-degrade", SessionToken: "PSE-TEST",
		TaskRoot: workspace, StartedAt: time.Now().Add(-time.Minute),
	}
	processors := map[string]*normalize.ClaudeTranscriptProcessor{}

	// Live poll (dryRun=false): events parse and queue; delivery fails at the
	// 429ing server, which must be irrelevant to the count.
	parsed, consumed := pollClaudeTranscripts(session, resolvePath(workspace),
		session.StartedAt.Add(-2*time.Minute), processors, false, false)

	if parsed == 0 {
		t.Fatalf("precondition: parser produced no events from %d bytes", consumed)
	}

	// Drive the state machine exactly as RunClaudeWatcher does, over many polls
	// with a completely dead network.
	degraded := false
	var bytesSinceEvent int64
	for i := 0; i < 200; i++ {
		degraded, bytesSinceEvent = claudeDegradationStep(degraded, parsed, consumed, bytesSinceEvent)
		if degraded {
			t.Fatalf("a send outage tripped the PARSER-break detector after %d poll(s) "+
				"(parsed=%d) — this is bug 4: hooks take over a window they cannot replay", i+1, parsed)
		}
	}
}

// TestDegradationStillTripsOnRealParserBreak is the other half: the detector
// must keep working for what it is actually for. Genuine parser breakage
// (bytes consumed, zero events parsed) must still degrade so hooks take over.
func TestDegradationStillTripsOnRealParserBreak(t *testing.T) {
	tests := []struct {
		name         string
		parsed       int
		consumed     int64
		wantDegraded bool
	}{
		{
			name:         "parses reset the counter no matter how many bytes",
			parsed:       1,
			consumed:     claudeDegradedByteThreshold * 4,
			wantDegraded: false,
		},
		{
			name:         "bytes with zero parses below the threshold stay healthy",
			parsed:       0,
			consumed:     claudeDegradedByteThreshold / 2,
			wantDegraded: false,
		},
		{
			name:         "bytes with zero parses past the threshold degrade",
			parsed:       0,
			consumed:     claudeDegradedByteThreshold + 1,
			wantDegraded: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := claudeDegradationStep(false, tc.parsed, tc.consumed, 0)
			if got != tc.wantDegraded {
				t.Errorf("degraded = %v, want %v", got, tc.wantDegraded)
			}
		})
	}
}
