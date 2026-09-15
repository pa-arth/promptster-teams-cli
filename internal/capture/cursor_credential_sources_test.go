package capture

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/redact"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// TestMain keeps every test in this package off the developer's real
// ~/.cursor/auth.json. A test that wants the source sets its own dir.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "promptster-no-cursor-agent-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv(cursorAgentDirEnv, filepath.Join(dir, "absent"))
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// agentLogin writes cursor-agent's auth.json (with the apiKey and bedrock
// neighbours the real schema has) and, when authID is non-empty, a
// cli-config.json whose authInfo pairs authID with email.
func agentLogin(t *testing.T, token, authID, email string) string {
	t.Helper()
	dir := t.TempDir()
	auth := fmt.Sprintf(`{"accessToken":%q,"refreshToken":%q,"apiKey":"agent-api-key-secret","bedrockCredentials":{"accessKey":"bedrock-access-secret","secretKey":"bedrock-secret-key"}}`, token, token)
	writeAgentFile(t, dir, cursorAgentAuthFileName, auth)
	if authID != "" {
		writeAgentFile(t, dir, cursorAgentCLIConfigFileName, fmt.Sprintf(`{"version":1,"authInfo":{"authId":%q,"email":%q,"userId":42,"displayName":"Private Person"}}`, authID, email))
	}
	t.Setenv(cursorAgentDirEnv, dir)
	return dir
}

func writeAgentFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func agentToken(t *testing.T, sub string, exp int64) string {
	return jwtWithClaims(t, fmt.Sprintf(`{"sub":%q,"exp":%d}`, sub, exp))
}

// currentCycleComplete drops the previous-cycle snapshot each poll also stages
// (#224), so counts are per login for fakeVendor's current cycle.
func currentCycleComplete(evs []event.Event) []map[string]interface{} {
	var out []map[string]interface{}
	for _, d := range vendorSnapshotData(evs, CursorVendorSnapshotStatusComplete) {
		if d["billingCycleStartsAt"] == "2026-08-01T00:00:00.000Z" {
			out = append(out, d)
		}
	}
	return out
}

func refsOf(snaps []map[string]interface{}) map[string]bool {
	out := map[string]bool{}
	for _, s := range snaps {
		out[fmt.Sprint(s["accountRef"])] = true
	}
	return out
}

// cursor-agent signed into a different login than the IDE: two snapshots, and
// the login map learns both.
func TestCursorVendorAgentAuthFileDifferentLogin(t *testing.T) {
	for name, ideAbsent := range map[string]bool{"ide signed in": false, "no ide store": true} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
			resolver := permittedCursorPolicy(t)
			client, calls := fakeVendor(t)
			evs := captureVendorEvents(t)

			var ide string
			if ideAbsent {
				t.Setenv(cursorStateDBEnv, filepath.Join(t.TempDir(), "missing.vscdb"))
			} else {
				ide = ideLogin(t, "auth0|ide_login", "ide@example.com", 4102444800)
			}
			agent := agentToken(t, "auth0|agent_login", 4102444800)
			agentLogin(t, agent, "auth0|agent_login", "Agent@Example.com")
			pollCursorVendorUsage("dev", resolver, client, time.Now())

			complete := currentCycleComplete(*evs)
			absent := vendorSnapshotData(*evs, CursorVendorSnapshotStatusAbsent)
			wantRefs := map[string]bool{cursorAccountRef(agent): true}
			if !ideAbsent {
				wantRefs[cursorAccountRef(ide)] = true
			}
			if len(complete) != len(wantRefs) || fmt.Sprint(refsOf(complete)) != fmt.Sprint(wantRefs) || len(absent) != 0 {
				t.Fatalf("complete=%v absent=%v, want one complete per login %v and no absence", complete, absent, wantRefs)
			}
			if calls[agent] != 1 || (!ideAbsent && calls[ide] != 1) {
				t.Fatalf("collections=%v, want one per login", calls)
			}
			if ref, ok := cursorHookAccountRef(stopPayload(t, "agent@example.com")); !ok || ref != cursorAccountRef(agent) {
				t.Fatalf("agent login: ref=%q ok=%v, want %q", ref, ok, cursorAccountRef(agent))
			}
			if !ideAbsent {
				if ref, _ := cursorHookAccountRef(stopPayload(t, "ide@example.com")); ref != cursorAccountRef(ide) {
					t.Fatalf("ide login: ref=%q, want %q", ref, cursorAccountRef(ide))
				}
			}
		})
	}
}

// The same login in both stores is ONE account: one snapshot, one collection.
// A stale expired copy in one store does not produce an absence for a login the
// other store collected.
func TestCursorVendorAgentAuthFileSameLoginDedupes(t *testing.T) {
	for name, ideExp := range map[string]int64{"both valid": 4102444800, "ide copy expired": 1000000000} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
			resolver := permittedCursorPolicy(t)
			client, calls := fakeVendor(t)
			evs := captureVendorEvents(t)

			ide := ideLogin(t, "auth0|shared", "shared@example.com", ideExp)
			agent := agentToken(t, "auth0|shared", 4102444801) // different bytes, same sub
			agentLogin(t, agent, "auth0|shared", "shared@example.com")
			pollCursorVendorUsage("dev", resolver, client, time.Now())

			complete := currentCycleComplete(*evs)
			absent := vendorSnapshotData(*evs, CursorVendorSnapshotStatusAbsent)
			if len(complete) != 1 || complete[0]["accountRef"] != cursorAccountRef(ide) || len(absent) != 0 {
				t.Fatalf("complete=%v absent=%v, want exactly one snapshot for the shared login", complete, absent)
			}
			if calls[ide]+calls[agent] != 1 {
				t.Fatalf("collections=%v, want one", calls)
			}
			if n := len(loadCursorAccountLogins().Logins); n != 1 {
				t.Fatalf("login map has %d logins, want 1", n)
			}
		})
	}
}

// Absent, malformed, tokenless, sub-less or expired auth.json: no crash, the
// IDE login collects exactly as before, and only a known expired login emits.
func TestCursorVendorAgentAuthFileBadStatesLeaveIDEUnaffected(t *testing.T) {
	cases := map[string]struct {
		setup       func(t *testing.T) string // returns the agent token, if any
		wantExpired bool
	}{
		"absent": {setup: func(t *testing.T) string { t.Setenv(cursorAgentDirEnv, t.TempDir()); return "" }},
		"malformed": {setup: func(t *testing.T) string {
			dir := t.TempDir()
			writeAgentFile(t, dir, cursorAgentAuthFileName, `{"accessToken": "trunc`)
			t.Setenv(cursorAgentDirEnv, dir)
			return ""
		}},
		"no token": {setup: func(t *testing.T) string {
			dir := t.TempDir()
			writeAgentFile(t, dir, cursorAgentAuthFileName, `{"apiKey":"agent-api-key-secret"}`)
			t.Setenv(cursorAgentDirEnv, dir)
			return ""
		}},
		"no sub": {setup: func(t *testing.T) string {
			tok := jwtWithClaims(t, `{"exp":4102444800}`)
			agentLogin(t, tok, "", "")
			return tok
		}},
		"expired": {wantExpired: true, setup: func(t *testing.T) string {
			tok := agentToken(t, "auth0|agent_expired", 1000000000)
			agentLogin(t, tok, "auth0|agent_expired", "expired@example.com")
			return tok
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
			resolver := permittedCursorPolicy(t)
			client, calls := fakeVendor(t)
			evs := captureVendorEvents(t)
			ide := ideLogin(t, "auth0|ide_login", "ide@example.com", 4102444800)
			agent := tc.setup(t)
			pollCursorVendorUsage("dev", resolver, client, time.Now())

			complete := currentCycleComplete(*evs)
			if len(complete) != 1 || complete[0]["accountRef"] != cursorAccountRef(ide) || calls[ide] != 1 || len(calls) != 1 {
				t.Fatalf("complete=%v calls=%v, want the IDE login collected once and nothing else", complete, calls)
			}
			absent := vendorSnapshotData(*evs, CursorVendorSnapshotStatusAbsent)
			if tc.wantExpired {
				if len(absent) != 1 || absent[0]["accountRef"] != cursorAccountRef(agent) || absent[0]["absenceReason"] != string(CursorVendorAbsenceCredentialExpired) {
					t.Fatalf("absent=%v, want one credential_expired naming the agent login", absent)
				}
			} else if len(absent) != 0 {
				t.Fatalf("absent=%v, want none", absent)
			}
			if n := len(loadCursorAccountLogins().Logins); n != 1 {
				t.Fatalf("login map has %d logins, want only the IDE's", n)
			}
		})
	}
}

// authInfo naming a DIFFERENT login than the token (cursor-agent re-logged and
// cli-config lags, say) must not attribute the token's account to that email.
func TestCursorVendorAgentAuthFileMismatchedAuthInfoCollectsWithoutAttribution(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	resolver := permittedCursorPolicy(t)
	client, calls := fakeVendor(t)
	_ = captureVendorEvents(t)
	ide := ideLogin(t, "auth0|ide_login", "ide@example.com", 4102444800)
	agent := agentToken(t, "auth0|agent_login", 4102444800)
	agentLogin(t, agent, "auth0|someone_else", "someone.else@example.com")
	pollCursorVendorUsage("dev", resolver, client, time.Now())

	if calls[agent] != 1 || calls[ide] != 1 {
		t.Fatalf("collections=%v, want both logins collected", calls)
	}
	if ref, _ := cursorHookAccountRef(stopPayload(t, "someone.else@example.com")); !strings.HasPrefix(ref, cursorAccountRefUnreadablePrefix) {
		t.Fatalf("mismatched authInfo attributed: ref=%q", ref)
	}
}

// No token, apiKey, bedrock secret, sub or email from cursor-agent's files may
// appear in any emitted byte, the login map, or a debug log line.
func TestCursorVendorAgentAuthFileNeverEmitsSecretsOrEmail(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	t.Setenv("PROMPTSTER_DEBUG", "1")
	resolver := permittedCursorPolicy(t)
	client, _ := fakeVendor(t)
	evs := captureVendorEvents(t)
	ideLogin(t, "auth0|ide_private", "ide.private@example.com", 1000000000) // expired: exercises the debug log line
	agent := agentToken(t, "auth0|agent_private", 4102444800)
	agentLogin(t, agent, "auth0|agent_private", "Agent.Private@Example.com")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := os.Stderr
	os.Stderr = w
	pollCursorVendorUsage("dev", resolver, client, time.Now())
	os.Stderr = stderr
	_ = w.Close()
	logged, _ := io.ReadAll(r)

	if len(currentCycleComplete(*evs)) != 1 {
		t.Fatalf("precondition: agent login not collected: %v", *evs)
	}
	if !bytes.Contains(logged, []byte(string(cursorSourceIDEStateDB))) {
		t.Fatalf("precondition: the debug source line was not written: %q", logged)
	}
	banned := []string{agent, "agent-api-key-secret", "bedrock-access-secret", "bedrock-secret-key",
		"agent_private", "ide_private", "Agent.Private", "agent.private", "ide.private", "example.com", "Private Person",
		hex.EncodeToString(state.CursorAttributionKey())}
	var blobs [][]byte
	for _, e := range *evs {
		redact.ProjectEvent(&e, false)
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		blobs = append(blobs, b)
	}
	onDisk, err := os.ReadFile(cursorAccountReadingPath())
	if err != nil {
		t.Fatal(err)
	}
	blobs = append(blobs, onDisk, logged)
	for _, b := range blobs {
		for _, s := range banned {
			if bytes.Contains(b, []byte(s)) {
				t.Fatalf("leaked %q in: %s", s, b)
			}
		}
	}
}
