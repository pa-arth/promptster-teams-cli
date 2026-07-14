package sign

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// setupChainTest points state + buffer at a temp dir and provisions a keypair,
// returning the parsed public key for signature verification.
func setupChainTest(t *testing.T) ed25519.PublicKey {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))

	pubB64, err := GenerateSessionKeypair()
	if err != nil {
		t.Fatalf("generate keypair: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil {
		t.Fatalf("decode pubkey: %v", err)
	}
	return ed25519.PublicKey(raw)
}

func appendEvent(t *testing.T, kind, sessionID string) {
	t.Helper()
	ev := event.NewEvent(kind, sessionID)
	if err := AppendEventToLocalBuffer(&ev, false); err != nil {
		t.Fatalf("append %s/%s: %v", kind, sessionID, err)
	}
}

// readBuffer returns every event in the ledger, in file order.
func readBuffer(t *testing.T) []event.Event {
	t.Helper()
	data, err := os.ReadFile(state.HookBufferPath())
	if err != nil {
		t.Fatalf("read buffer: %v", err)
	}
	var out []event.Event
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev event.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("unmarshal buffer line: %v", err)
		}
		out = append(out, ev)
	}
	return out
}

// bySession groups ledger events by sessionId, preserving file order within
// each group. This is how any future verifier must walk the chain.
func bySession(events []event.Event) map[string][]event.Event {
	groups := map[string][]event.Event{}
	for _, ev := range events {
		groups[ev.SessionID] = append(groups[ev.SessionID], ev)
	}
	return groups
}

// verifyChain asserts a session's events form one intact, individually-valid
// chain: first event starts a segment, each later event links to its
// predecessor, and every signature verifies.
func verifyChain(t *testing.T, pub ed25519.PublicKey, sessionID string, evs []event.Event) {
	t.Helper()
	for i, ev := range evs {
		if i == 0 {
			if ev.PrevSig != "" {
				t.Errorf("%s[0]: prevSig = %q, want \"\" (a session's first event starts a new segment)", sessionID, ev.PrevSig)
			}
		} else if ev.PrevSig != evs[i-1].Sig {
			t.Errorf("%s[%d]: prevSig = %q, want %q (must link to the previous event of the SAME session)", sessionID, i, ev.PrevSig, evs[i-1].Sig)
		}
		msg, err := BuildSigningMessage(ev, ev.PrevSig)
		if err != nil {
			t.Fatalf("build signing message: %v", err)
		}
		sig, err := hex.DecodeString(ev.Sig)
		if err != nil {
			t.Fatalf("%s[%d]: decode sig: %v", sessionID, i, err)
		}
		if !ed25519.Verify(pub, msg, sig) {
			t.Errorf("%s[%d]: signature does not verify", sessionID, i)
		}
	}
}

// TestInterleavedSessionsProduceIndependentChains is the headline regression.
// Before per-session chaining, every event linked to whatever was appended
// last, so B1 chained to A1 and the two sessions were welded into one
// unwalkable chain. The load-bearing assertion is B1.PrevSig == "".
func TestInterleavedSessionsProduceIndependentChains(t *testing.T) {
	pub := setupChainTest(t)

	appendEvent(t, "prompt", "sess-a")
	appendEvent(t, "prompt", "sess-b")
	appendEvent(t, "command", "sess-a")
	appendEvent(t, "command", "sess-b")
	appendEvent(t, "prompt", "sess-a")

	groups := bySession(readBuffer(t))
	if len(groups) != 2 {
		t.Fatalf("got %d sessions in ledger, want 2", len(groups))
	}
	if got := len(groups["sess-a"]); got != 3 {
		t.Errorf("sess-a has %d events, want 3", got)
	}
	if got := len(groups["sess-b"]); got != 2 {
		t.Errorf("sess-b has %d events, want 2", got)
	}
	for sid, evs := range groups {
		verifyChain(t, pub, sid, evs)
	}

	// Explicit: B's first event must NOT chain to A's first event, even though
	// A1 was the last line in the ledger when B1 was appended. This is exactly
	// what the old global-tip code did.
	if groups["sess-b"][0].PrevSig == groups["sess-a"][0].Sig {
		t.Error("sess-b chained to sess-a's tip — sessions are welded together")
	}
}

// TestDeviceScopedEventsShareTheDeviceChain pins that presence/config_census,
// which stay device-scoped by design, chain to each other and never to
// transcript sessions interleaved between them.
func TestDeviceScopedEventsShareTheDeviceChain(t *testing.T) {
	pub := setupChainTest(t)
	const deviceID = "dev-eaadff93e23fe6d4"

	appendEvent(t, "presence", deviceID)
	appendEvent(t, "prompt", "sess-a")
	appendEvent(t, "config_census", deviceID)
	appendEvent(t, "prompt", "sess-a")
	appendEvent(t, "presence", deviceID)

	groups := bySession(readBuffer(t))
	device := groups[deviceID]
	if len(device) != 3 {
		t.Fatalf("device chain has %d events, want 3", len(device))
	}
	verifyChain(t, pub, deviceID, device)
	verifyChain(t, pub, "sess-a", groups["sess-a"])
}

// TestConcurrentAppendsAcrossSessions mirrors the four real emitters (both
// watchers, presence, census) hammering one ledger. Run under -race. Proves the
// flock serializes the read->sign->append->commit sequence: no lost appends and
// every chain intact.
func TestConcurrentAppendsAcrossSessions(t *testing.T) {
	pub := setupChainTest(t)

	sessions := []string{"sess-a", "sess-b", "dev-abc123", "sess-codex"}
	const perSession = 25

	var wg sync.WaitGroup
	for _, sid := range sessions {
		wg.Add(1)
		go func(sid string) {
			defer wg.Done()
			for i := 0; i < perSession; i++ {
				ev := event.NewEvent("command", sid)
				if err := AppendEventToLocalBuffer(&ev, false); err != nil {
					t.Errorf("append %s: %v", sid, err)
					return
				}
			}
		}(sid)
	}
	wg.Wait()

	all := readBuffer(t)
	if want := len(sessions) * perSession; len(all) != want {
		t.Fatalf("ledger has %d events, want %d — appends were lost or duplicated under concurrency", len(all), want)
	}
	groups := bySession(all)
	for _, sid := range sessions {
		if got := len(groups[sid]); got != perSession {
			t.Errorf("%s has %d events, want %d", sid, got, perSession)
		}
		verifyChain(t, pub, sid, groups[sid])
	}
}

// TestChainStateRebuildFromLegacyBuffer pins the upgrade path. A pre-upgrade
// ledger has every event stamped with the device id and no index alongside it.
// The rebuild must reproduce the legacy device-wide tip exactly, so the old
// chain continues unbroken, while a genuinely new session still starts a fresh
// segment.
func TestChainStateRebuildFromLegacyBuffer(t *testing.T) {
	pub := setupChainTest(t)
	const deviceID = "dev-eaadff93e23fe6d4"

	// Build a legacy-shaped ledger: all one sessionId, chained, then drop the
	// index to simulate upgrading into a buffer that predates it.
	appendEvent(t, "presence", deviceID)
	appendEvent(t, "config_census", deviceID)
	legacy := readBuffer(t)
	legacyTip := legacy[len(legacy)-1].Sig

	if err := os.Remove(state.ChainStatePath()); err != nil {
		t.Fatalf("remove chain state: %v", err)
	}

	appendEvent(t, "presence", deviceID)
	appendEvent(t, "prompt", "sess-new")

	groups := bySession(readBuffer(t))
	if got := groups[deviceID][2].PrevSig; got != legacyTip {
		t.Errorf("post-upgrade device event prevSig = %q, want the legacy tip %q — the legacy chain was broken by the upgrade", got, legacyTip)
	}
	if got := groups["sess-new"][0].PrevSig; got != "" {
		t.Errorf("new session's first event prevSig = %q, want \"\"", got)
	}
	verifyChain(t, pub, deviceID, groups[deviceID])
}

// TestChainStateCorruptRebuilds: a garbage index must self-heal from the
// ledger rather than fork the chain.
func TestChainStateCorruptRebuilds(t *testing.T) {
	pub := setupChainTest(t)

	appendEvent(t, "prompt", "sess-a")
	tip := readBuffer(t)[0].Sig

	if err := os.WriteFile(state.ChainStatePath(), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("corrupt chain state: %v", err)
	}

	appendEvent(t, "command", "sess-a")
	evs := bySession(readBuffer(t))["sess-a"]
	if evs[1].PrevSig != tip {
		t.Errorf("prevSig = %q, want %q — corrupt index must rebuild from the ledger, not fork", evs[1].PrevSig, tip)
	}
	verifyChain(t, pub, "sess-a", evs)
}

// TestAppendSurvivesTornLastLine pins a fix over the old behaviour. The old
// readLastChainSig json.Unmarshal'd the last line and returned an error on a
// torn write, which the caller swallowed into prevSig="" — silently forking the
// chain. Skipping unparseable lines means we still find the last well-formed
// tip.
func TestAppendSurvivesTornLastLine(t *testing.T) {
	setupChainTest(t)

	appendEvent(t, "prompt", "sess-a")
	firstTip := readBuffer(t)[0].Sig
	appendEvent(t, "command", "sess-a")

	// Truncate mid-JSON, mimicking a crash during the second append, and drop
	// the index so the tip must come from the ledger.
	raw, err := os.ReadFile(state.HookBufferPath())
	if err != nil {
		t.Fatalf("read buffer: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	torn := lines[0] + "\n" + lines[1][:len(lines[1])/2]
	if err := os.WriteFile(state.HookBufferPath(), []byte(torn+"\n"), 0o600); err != nil {
		t.Fatalf("write torn buffer: %v", err)
	}
	if err := os.Remove(state.ChainStatePath()); err != nil {
		t.Fatalf("remove chain state: %v", err)
	}

	appendEvent(t, "command", "sess-a")

	raw, err = os.ReadFile(state.HookBufferPath())
	if err != nil {
		t.Fatalf("re-read buffer: %v", err)
	}
	lines = strings.Split(strings.TrimSpace(string(raw)), "\n")
	var last event.Event
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatalf("unmarshal appended event: %v", err)
	}
	if last.PrevSig == "" {
		t.Error("prevSig = \"\" after a torn line — the chain silently forked (the old bug)")
	}
	if last.PrevSig != firstTip {
		t.Errorf("prevSig = %q, want %q (the last well-formed event)", last.PrevSig, firstTip)
	}
}

// TestIndexWriteFailureDoesNotStrandStaleTip pins the subtlest failure here.
//
// A stale index is not self-healing: it stays valid JSON, so readChainState
// keeps accepting it. If a write fails (disk full) and we left the old file in
// place, EVERY later event of that session would re-link to the same frozen
// tip — a silent star, not a chain, and not a one-off fork. So a failed write
// must drop the index and let the ledger (which already holds the event)
// re-derive it.
//
// Blocks the write by parking a directory on the temp path writeChainState
// renames from, which leaves the real index file readable and removable —
// exactly the disk-full shape.
func TestIndexWriteFailureDoesNotStrandStaleTip(t *testing.T) {
	pub := setupChainTest(t)

	appendEvent(t, "prompt", "sess-a")

	blocker := state.ChainStatePath() + ".tmp"
	if err := os.Mkdir(blocker, 0o700); err != nil {
		t.Fatalf("park blocker dir: %v", err)
	}
	if _, err := os.Stat(state.ChainStatePath()); err != nil {
		t.Fatalf("index should still exist and be readable: %v", err)
	}

	// Write fails here; the fix drops the now-unmaintainable index.
	appendEvent(t, "command", "sess-a")

	if _, err := os.Stat(state.ChainStatePath()); !os.IsNotExist(err) {
		t.Error("index survived a failed write — a stale tip will be trusted forever")
	}

	// Unblock so the index can be maintained again, then keep appending.
	if err := os.Remove(blocker); err != nil {
		t.Fatalf("remove blocker: %v", err)
	}
	appendEvent(t, "command", "sess-a")

	evs := bySession(readBuffer(t))["sess-a"]
	if len(evs) != 3 {
		t.Fatalf("got %d events, want 3", len(evs))
	}
	// The regression: without dropping the index, evs[2] would re-link to
	// evs[0].Sig — the same parent as evs[1] — forking the chain.
	if evs[2].PrevSig == evs[1].PrevSig {
		t.Error("two events share a parent — the chain forked off a stale tip")
	}
	verifyChain(t, pub, "sess-a", evs)
}

// TestIncompleteRebuildIsNotPersisted: if the scan cannot see the whole ledger,
// the tips it found are a lower bound. Writing them back would freeze a partial
// index in place and stop future rebuilds, turning a transient read problem into
// a permanent silent fork.
func TestIncompleteRebuildIsNotPersisted(t *testing.T) {
	setupChainTest(t)

	appendEvent(t, "prompt", "sess-a")
	if err := os.Remove(state.ChainStatePath()); err != nil {
		t.Fatalf("remove index: %v", err)
	}

	// Prepend a line past the scanner's token limit so the scan aborts before
	// reaching sess-a's real event.
	existing, err := os.ReadFile(state.HookBufferPath())
	if err != nil {
		t.Fatalf("read buffer: %v", err)
	}
	oversized := append([]byte(`{"sessionId":"x","sig":"`), bytes.Repeat([]byte("a"), 17<<20)...)
	oversized = append(oversized, []byte("\"}\n")...)
	if err := os.WriteFile(state.HookBufferPath(), append(oversized, existing...), 0o600); err != nil {
		t.Fatalf("write oversized buffer: %v", err)
	}

	cs, complete := rebuildChainStateFromBuffer(state.HookBufferPath())
	if complete {
		t.Error("rebuild reported complete despite aborting mid-scan")
	}
	if len(cs.Sessions) != 0 {
		t.Errorf("rebuild saw %d sessions past the oversized line, want 0", len(cs.Sessions))
	}

	appendEvent(t, "command", "sess-a")
	if _, err := os.Stat(state.ChainStatePath()); !os.IsNotExist(err) {
		t.Error("a partial index was persisted — future rebuilds are now suppressed")
	}
}

// TestChainStatePrunesByTTLAndCap: stale sessions age out and the index stays
// bounded, but the session currently emitting can never evict itself — setTip
// stamps it fresh before prune runs.
func TestChainStatePrunesByTTLAndCap(t *testing.T) {
	nowMs := time.Now().UnixMilli()
	cs := chainState{V: chainStateVersion, Sessions: map[string]chainEntry{}}

	cs.Sessions["stale"] = chainEntry{LastSig: "aa", TsMs: nowMs - chainStateTTL.Milliseconds() - 1000}
	cs.Sessions["fresh"] = chainEntry{LastSig: "bb", TsMs: nowMs}
	cs.prune(nowMs)

	if _, ok := cs.Sessions["stale"]; ok {
		t.Error("stale session survived TTL prune")
	}
	if _, ok := cs.Sessions["fresh"]; !ok {
		t.Error("fresh session was pruned")
	}

	// Overflow the cap with entries older than the active one.
	for i := 0; i < chainStateMaxEntries+50; i++ {
		cs.Sessions[string(rune('a'+i%26))+strings.Repeat("x", i)] = chainEntry{LastSig: "cc", TsMs: nowMs - 1000}
	}
	cs.setTip("active", "dd", nowMs)
	cs.prune(nowMs)

	if len(cs.Sessions) > chainStateMaxEntries {
		t.Errorf("index has %d entries, want <= %d", len(cs.Sessions), chainStateMaxEntries)
	}
	if _, ok := cs.Sessions["active"]; !ok {
		t.Error("the actively-emitting session was evicted — setTip must stamp it fresh before prune")
	}
}

// TestSigningMessageShape pins the exact field order and arity of the signing
// message, which is mirrored byte-for-byte in the backend's TS implementation
// (see canonicalJSON in signing.go). The two must change in lockstep, and
// nothing else in the build fails if they drift — so this test is the only
// mechanical guard.
//
// In particular this fails if anyone adds a field (e.g. deviceId) to the
// message without a coordinated PST-EVT-V2 rollout across both sides.
func TestSigningMessageShape(t *testing.T) {
	ev := event.Event{
		ID:        "11111111-2222-3333-4444-555555555555",
		SessionID: "sess-a",
		Ts:        "2026-07-14T12:00:00Z",
		Kind:      "prompt",
		Source:    "claude-code",
		V:         1,
		Data:      map[string]interface{}{"b": "2", "a": "1"},
	}
	msg, err := BuildSigningMessage(ev, "deadbeef")
	if err != nil {
		t.Fatalf("build signing message: %v", err)
	}
	parts := strings.Split(string(msg), "\n")

	if len(parts) != 10 {
		t.Fatalf("signing message has %d parts, want 10 — a field was added, removed, or reordered; this MUST stay in lockstep with the backend's TS signing implementation", len(parts))
	}
	want := []struct {
		idx  int
		name string
		val  string
	}{
		{0, "version", "PST-EVT-V1"},
		{1, "id", ev.ID},
		{2, "sessionId", ev.SessionID},
		{3, "ts", ev.Ts},
		{4, "kind", ev.Kind},
		{5, "source", ev.Source},
		{6, "v", "1"},
		{8, "prevSig", "deadbeef"},
		{9, "trailer", ""},
	}
	for _, w := range want {
		if parts[w.idx] != w.val {
			t.Errorf("part[%d] (%s) = %q, want %q", w.idx, w.name, parts[w.idx], w.val)
		}
	}
	if len(parts[7]) != 64 {
		t.Errorf("part[7] (dataHash) = %q, want 64 hex chars", parts[7])
	}
	if _, err := hex.DecodeString(parts[7]); err != nil {
		t.Errorf("part[7] (dataHash) is not hex: %v", err)
	}
}
