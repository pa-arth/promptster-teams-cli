package capture

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/outbox"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// TestQueueClaudeWatchEventNeverTouchesTheNetwork pins the parse/send split.
//
// This REPLACES TestIngestClaudeWatchEventToleratesRejection, whose contract no
// longer exists on this path. That test pinned "a 400 counts as handled, a 5xx
// counts as a send failure" on the parse loop — a rule that only mattered
// because the loop POSTed inline and fed its send result to the PARSER-break
// detector, which is bug 4. Delivery is now the outbox's job: the 400-vs-5xx
// rules are pinned in TestDrainAdvancesPastRejections and
// TestDrainRetriesServerErrorsWithBackoff, and this path's contract is simply
// "it queues, it does not send".
func TestQueueClaudeWatchEventNeverTouchesTheNetwork(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(tmp, "outbox.jsonl"))

	// Provision a signing key so the ledger actually signs — without one
	// AppendEventToLocalBuffer appends unsigned and the ordering assertion below
	// would be vacuous.
	pubB64, err := sign.GenerateSessionKeypair()
	if err != nil {
		t.Fatal(err)
	}

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	// SessionID comes from the transcript, DeviceID from the environment (#48).
	// Keep them distinct here so a regression that collapses one into the other
	// cannot pass this test.
	session := Session{DeviceID: "device-abc", SessionToken: "PSE-TEST", TaskRoot: tmp}
	queueClaudeWatchEvent(event.NewEvent("subagent_usage", "transcript-session-1"), session, false)

	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("parse path must not POST — delivery is the outbox's job; got %d request(s)", got)
	}
	// The event must survive the failing backend rather than being dropped, which
	// is bug 2: the old path POSTed here and lost the event on any non-2xx.
	if n := outbox.PendingCount(); n != 1 {
		t.Fatalf("event must be durably queued despite the backend being down; pending = %d, want 1", n)
	}

	// Guard the rebase seam. This funnel was renamed and rewired while #48 was
	// adding the DeviceID stamp to the version it replaced; resolving in favour
	// of either side alone silently drops the other. Assert BOTH survived, from
	// the bytes actually queued.
	queued, err := os.ReadFile(state.OutboxPath())
	if err != nil {
		t.Fatal(err)
	}
	var got event.Event
	if err := json.Unmarshal(queued[:bytes.IndexByte(queued, '\n')], &got); err != nil {
		t.Fatal(err)
	}
	if got.DeviceID != "device-abc" {
		t.Errorf("DeviceID = %q, want %q — the #48 stamp was lost in the merge", got.DeviceID, "device-abc")
	}
	if got.SessionID != "transcript-session-1" {
		t.Errorf("SessionID = %q, want %q — device identity collapsed into the session id (#48)", got.SessionID, "transcript-session-1")
	}
	// Signing still works end-to-end and the queued bytes verify. This does NOT
	// guard the stamp ordering — DeviceID is deliberately outside
	// BuildSigningMessage, so a late stamp would verify just fine (measured).
	if got.Sig == "" {
		t.Fatal("queued event is unsigned")
	}
	pub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := sign.BuildSigningMessage(got, got.PrevSig)
	if err != nil {
		t.Fatal(err)
	}
	rawSig, err := hex.DecodeString(got.Sig)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(pub, msg, rawSig) {
		t.Error("queued bytes do not verify against their own signature")
	}

	// THIS is what pins the ordering. AppendEventToLocalBuffer writes the ledger
	// copy, so a DeviceID stamped after it leaves the signed audit trail saying
	// "no device" while the wire copy names one — the two artifacts disagreeing
	// about who produced the event. Measured: stamping late yields ledger=absent,
	// outbox=present.
	ledger, err := os.ReadFile(state.HookBufferPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(ledger, []byte("device-abc")) {
		t.Error("ledger copy carries no DeviceID — the stamp must land BEFORE sign.AppendEventToLocalBuffer, " +
			"or the audit trail and the wire disagree about the device")
	}
}
