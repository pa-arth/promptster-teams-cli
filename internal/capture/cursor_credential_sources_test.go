package capture

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
	"github.com/pa-arth/promptster-teams-cli/internal/redact"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

const (
	subIDE   = "auth0|user_ide_login"
	subAgent = "auth0|user_agent_login"

	emptyUsage = `{"totalUsageEventsCount":0,"usageEventsDisplay":[]}`
	oneRow     = `{"totalUsageEventsCount":1,"usageEventsDisplay":[{"timestamp":"1785542401000","model":"default","kind":"x","conversationId":"conv-1","tokenUsage":{"inputTokens":1,"outputTokens":0,"cacheReadTokens":0,"totalCents":1}}]}`
)

type securityFunc = func(context.Context, ...string) ([]byte, error)

// agentSourceEnv isolates one test: private state dir, empty HOME and config
// dirs, cursor-agent sources on for every CI OS, and a FAKE `security`. No test
// here reads a real keychain or a real Cursor store.
func agentSourceEnv(t *testing.T, security securityFunc) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("CURSOR_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	prevSupported, prevRead, prevTimeout := cursorAgentSourcesSupported, cursorSecurityRead, cursorKeychainReadTimeout
	cursorAgentSourcesSupported, cursorSecurityRead = true, security
	t.Cleanup(func() {
		cursorAgentSourcesSupported, cursorSecurityRead, cursorKeychainReadTimeout = prevSupported, prevRead, prevTimeout
	})
	return home
}

func keychainReturns(t *testing.T, token string) securityFunc {
	return func(_ context.Context, args ...string) ([]byte, error) {
		want := []string{"find-generic-password", "-a", "cursor-user", "-s", "cursor-access-token", "-w"}
		if !reflect.DeepEqual(args, want) {
			t.Errorf("security args = %q, want %q", args, want)
		}
		return []byte(token + "\n"), nil
	}
}

func liveToken(t *testing.T, sub string) string {
	return jwtWithClaims(t, fmt.Sprintf(`{"sub":%q,"exp":4102444800}`, sub))
}

func expiredToken(t *testing.T, sub string) string {
	return jwtWithClaims(t, fmt.Sprintf(`{"sub":%q,"exp":1000000000}`, sub))
}

func ideStore(t *testing.T, token, email string) {
	values := map[string]string{cursorAuthAccessTokenKey: token, cursorAuthRefreshTokenKey: token}
	if email != "" {
		values[cursorAuthCachedEmailKey] = email
	}
	t.Setenv(cursorStateDBEnv, makeCursorStateDB(t, values))
}

// fakeVendor answers the three RPCs and counts GetCurrentPeriodUsage per bearer,
// which is one full collection per account.
func fakeVendor(t *testing.T, usage string) (*cursorVendorClient, map[string]int) {
	calls := map[string]int{}
	return &cursorVendorClient{base: cursorVendorAPIDefaultBase, http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case cursorMethodTeams:
			return response(`{}`), nil
		case cursorMethodCurrentUsage:
			calls[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]++
			return response(`{"billingCycleStart":"1785542400000","billingCycleEnd":"1788220800000"}`), nil
		case cursorMethodUsageEvents:
			return response(usage), nil
		}
		t.Fatalf("unexpected RPC %s", r.URL.Path)
		return nil, nil
	})}}, calls
}

func captureVendorEvents(t *testing.T) *[]event.Event {
	var got []event.Event
	prev := queueCursorVendorEvent
	queueCursorVendorEvent = func(ev event.Event) bool { got = append(got, ev); return true }
	t.Cleanup(func() { queueCursorVendorEvent = prev })
	return &got
}

// snapshotsByStatus returns accountRef -> snapshotId for one completion status,
// failing on a second event for the same account.
func snapshotsByStatus(t *testing.T, evs []event.Event, status string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, e := range evs {
		d := e.Data.(map[string]interface{})
		if e.Kind != "cursorVendorSnapshot" || d["status"] != status {
			continue
		}
		ref := d["accountRef"].(string)
		if _, dup := out[ref]; dup {
			t.Fatalf("two %s snapshots for account %s", status, ref)
		}
		out[ref] = d["snapshotId"].(string)
	}
	return out
}

func sourceKinds(sources []cursorCredentialSource) []cursorCredentialSourceKind {
	var out []cursorCredentialSourceKind
	for _, s := range sources {
		out = append(out, s.kind)
	}
	return out
}

func TestCursorSourcesDistinctSubsAreTwoAccounts(t *testing.T) {
	agentSourceEnv(t, keychainReturns(t, liveToken(t, subAgent)))
	ideStore(t, liveToken(t, subIDE), "")

	sources := readCursorCredentials()
	if got := sourceKinds(sources); !reflect.DeepEqual(got, []cursorCredentialSourceKind{cursorSourceIDE, cursorSourceAgentKeychain}) {
		t.Fatalf("sources = %v", got)
	}
	client, calls := fakeVendor(t, emptyUsage)
	evs := captureVendorEvents(t)
	pollCursorVendorAccounts("dev", client, time.Now(), sources)

	complete := snapshotsByStatus(t, *evs, CursorVendorSnapshotStatusComplete)
	if len(calls) != 2 || len(complete) != 2 {
		t.Fatalf("collections=%v complete=%v, want 2 accounts", calls, complete)
	}
	for _, sub := range []string{subIDE, subAgent} {
		if _, ok := complete[cursorAccountRef(liveToken(t, sub))]; !ok {
			t.Errorf("no snapshot for %s", sub)
		}
	}
	if abs := snapshotsByStatus(t, *evs, CursorVendorSnapshotStatusAbsent); len(abs) != 0 {
		t.Fatalf("unexpected absences: %v", abs)
	}
}

// The same login in the IDE and in cursor-agent — different token bytes, same
// sub — is ONE account, collected once.
func TestCursorSourcesSameSubIsOneAccount(t *testing.T) {
	agentSourceEnv(t, keychainReturns(t, jwtWithClaims(t, fmt.Sprintf(`{"sub":%q,"exp":4102444801}`, subIDE))))
	ideStore(t, liveToken(t, subIDE), "")

	client, calls := fakeVendor(t, emptyUsage)
	evs := captureVendorEvents(t)
	sources := readCursorCredentials()
	if len(sources) != 2 {
		t.Fatalf("sources = %v, want both read", sourceKinds(sources))
	}
	pollCursorVendorAccounts("dev", client, time.Now(), sources)
	total := 0
	for _, n := range calls {
		total += n
	}
	if total != 1 || len(snapshotsByStatus(t, *evs, CursorVendorSnapshotStatusComplete)) != 1 {
		t.Fatalf("collections=%v, want one account collected once", calls)
	}
}

// A missing keychain item, a timeout and a non-zero exit all mean "no
// cursor-agent login": the IDE is collected and the cycle reports no absence.
func TestCursorSourcesKeychainFailureIsAbsentSourceNotCycleError(t *testing.T) {
	for name, security := range map[string]securityFunc{
		"not found": func(context.Context, ...string) ([]byte, error) {
			return []byte("security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain."), errors.New("exit status 44")
		},
		"non-zero exit with output": func(context.Context, ...string) ([]byte, error) {
			return []byte(liveToken(t, subAgent)), errors.New("exit status 1")
		},
		"timeout": func(ctx context.Context, _ ...string) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	} {
		t.Run(name, func(t *testing.T) {
			agentSourceEnv(t, security)
			cursorKeychainReadTimeout = 20 * time.Millisecond
			ideStore(t, liveToken(t, subIDE), "")

			sources := readCursorCredentials()
			if got := sourceKinds(sources); !reflect.DeepEqual(got, []cursorCredentialSourceKind{cursorSourceIDE}) {
				t.Fatalf("sources = %v, want IDE only", got)
			}
			client, _ := fakeVendor(t, emptyUsage)
			evs := captureVendorEvents(t)
			pollCursorVendorAccounts("dev", client, time.Now(), sources)
			if c, a := snapshotsByStatus(t, *evs, CursorVendorSnapshotStatusComplete), snapshotsByStatus(t, *evs, CursorVendorSnapshotStatusAbsent); len(c) != 1 || len(a) != 0 {
				t.Fatalf("complete=%v absent=%v", c, a)
			}
		})
	}
}

func TestCursorAgentAuthFileIsASource(t *testing.T) {
	home := agentSourceEnv(t, func(context.Context, ...string) ([]byte, error) { return nil, errors.New("exit status 44") })
	ideStore(t, liveToken(t, subIDE), "")
	writeJSON(t, filepath.Join(home, ".cursor", "auth.json"), map[string]string{"accessToken": liveToken(t, subAgent), "refreshToken": "r"})

	sources := readCursorCredentials()
	if got := sourceKinds(sources); !reflect.DeepEqual(got, []cursorCredentialSourceKind{cursorSourceIDE, cursorSourceAgentFile}) {
		t.Fatalf("sources = %v", got)
	}
	if sources[1].err != nil || sources[1].cred.accountRef != cursorAccountRef(liveToken(t, subAgent)) {
		t.Fatalf("file source: ref=%q err=%v", sources[1].cred.accountRef, sources[1].err)
	}
}

// cli-config's authInfo is a cache refreshed apart from login; its email is
// only trusted for the token whose sub it names.
func TestCursorAgentEmailRequiresAuthIDMatchingSub(t *testing.T) {
	for authID, wantEmail := range map[string]bool{subAgent: true, "auth0|someone_else": false} {
		agentSourceEnv(t, keychainReturns(t, liveToken(t, subAgent)))
		cfg := t.TempDir()
		t.Setenv("CURSOR_CONFIG_DIR", cfg)
		ideStore(t, liveToken(t, subIDE), "")
		writeJSON(t, filepath.Join(cfg, "cli-config.json"), map[string]interface{}{"authInfo": map[string]string{"authId": authID, "email": "agent@example.org"}})

		sources := readCursorCredentials()
		if got := sources[1].cred.emailHMAC != ""; got != wantEmail {
			t.Errorf("authId %q: email used = %v, want %v", authID, got, wantEmail)
		}
	}
}

// The collision regression: two accounts with identical (empty) periods must
// not share a snapshot id, an absence id, or an absence event id.
func TestCursorVendorIDsAreScopedByAccount(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	a := buildCursorVendorSnapshot("aaaaaaaaaaaaaaaa", nil, start, end, nil, cursorVendorShapeRecord{})
	b := buildCursorVendorSnapshot("bbbbbbbbbbbbbbbb", nil, start, end, nil, cursorVendorShapeRecord{})
	if a.SnapshotID == b.SnapshotID {
		t.Fatal("two accounts with identical empty periods share a snapshotId")
	}
	if a.ContentSha256 != b.ContentSha256 {
		t.Fatal("contentSha256 moved with accountRef; the backend recomputes it without one")
	}
	now := time.Now()
	ea := buildCursorVendorAbsenceEvent("dev", "aaaaaaaaaaaaaaaa", CursorVendorAbsenceCredentialExpired, now, start, end, cursorVendorShapeRecord{})
	eb := buildCursorVendorAbsenceEvent("dev", "bbbbbbbbbbbbbbbb", CursorVendorAbsenceCredentialExpired, now, start, end, cursorVendorShapeRecord{})
	if ea.Data.(map[string]interface{})["snapshotId"] == eb.Data.(map[string]interface{})["snapshotId"] || ea.ID == eb.ID {
		t.Fatal("two accounts' absences share an id")
	}
}

func TestCursorVendorPollAbsencesArePerAccount(t *testing.T) {
	t.Run("both expired: one absence per account", func(t *testing.T) {
		agentSourceEnv(t, keychainReturns(t, expiredToken(t, subAgent)))
		ideStore(t, expiredToken(t, subIDE), "")
		client, calls := fakeVendor(t, emptyUsage)
		evs := captureVendorEvents(t)
		pollCursorVendorAccounts("dev", client, time.Now(), readCursorCredentials())

		abs := snapshotsByStatus(t, *evs, CursorVendorSnapshotStatusAbsent)
		refIDE, refAgent := cursorAccountRef(expiredToken(t, subIDE)), cursorAccountRef(expiredToken(t, subAgent))
		if len(calls) != 0 || len(abs) != 2 || abs[refIDE] == "" || abs[refAgent] == "" || abs[refIDE] == abs[refAgent] {
			t.Fatalf("calls=%v absences=%v", calls, abs)
		}
		if (*evs)[0].ID == (*evs)[1].ID {
			t.Fatal("absence event ids collide across accounts")
		}
	})
	t.Run("expired IDE copy of a login cursor-agent collected: no absence", func(t *testing.T) {
		agentSourceEnv(t, keychainReturns(t, liveToken(t, subIDE)))
		ideStore(t, expiredToken(t, subIDE), "")
		client, _ := fakeVendor(t, emptyUsage)
		evs := captureVendorEvents(t)
		pollCursorVendorAccounts("dev", client, time.Now(), readCursorCredentials())
		if c, a := snapshotsByStatus(t, *evs, CursorVendorSnapshotStatusComplete), snapshotsByStatus(t, *evs, CursorVendorSnapshotStatusAbsent); len(c) != 1 || len(a) != 0 {
			t.Fatalf("complete=%v absent=%v", c, a)
		}
	})
	t.Run("no IDE store, cursor-agent collected: no unknown absence", func(t *testing.T) {
		agentSourceEnv(t, keychainReturns(t, liveToken(t, subAgent)))
		t.Setenv(cursorStateDBEnv, filepath.Join(t.TempDir(), "missing.vscdb"))
		client, _ := fakeVendor(t, emptyUsage)
		evs := captureVendorEvents(t)
		pollCursorVendorAccounts("dev", client, time.Now(), readCursorCredentials())
		if c, a := snapshotsByStatus(t, *evs, CursorVendorSnapshotStatusComplete), snapshotsByStatus(t, *evs, CursorVendorSnapshotStatusAbsent); len(c) != 1 || len(a) != 0 {
			t.Fatalf("complete=%v absent=%v", c, a)
		}
	})
	t.Run("nothing readable: Phase A's unknown absence", func(t *testing.T) {
		agentSourceEnv(t, func(context.Context, ...string) ([]byte, error) { return nil, errors.New("exit status 44") })
		t.Setenv(cursorStateDBEnv, filepath.Join(t.TempDir(), "missing.vscdb"))
		evs := captureVendorEvents(t)
		pollCursorVendorAccounts("dev", nil, time.Now(), readCursorCredentials())
		if a := snapshotsByStatus(t, *evs, CursorVendorSnapshotStatusAbsent); len(a) != 1 || a[cursorAccountRefUnknown] == "" {
			t.Fatalf("absent=%v, want one unknown", a)
		}
	})
}

// Both logins land in the login map, a hook turn from either attributes, and
// neither token nor either email reaches an event, a log line or a state file.
func TestCursorMultiSourceLoginMapAndNoLeak(t *testing.T) {
	const ideEmail, agentEmail = "Ide.Login@Example.com", "Agent.Login@Example.org"
	ideTok, agentTok := liveToken(t, subIDE), liveToken(t, subAgent)
	agentSourceEnv(t, keychainReturns(t, agentTok))
	cfg := t.TempDir()
	t.Setenv("CURSOR_CONFIG_DIR", cfg)
	t.Setenv("PROMPTSTER_DEBUG", "1")
	ideStore(t, ideTok, ideEmail)
	writeJSON(t, filepath.Join(cfg, "cli-config.json"), map[string]interface{}{"authInfo": map[string]string{"authId": subAgent, "email": agentEmail}})

	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	prevStderr := os.Stderr
	os.Stderr = stderr
	client, calls := fakeVendor(t, oneRow)
	evs := captureVendorEvents(t)
	pollCursorVendorAccounts("dev", client, time.Now(), readCursorCredentials())
	os.Stderr = prevStderr
	_ = stderr.Close()
	if len(calls) != 2 {
		t.Fatalf("collections=%v, want 2", calls)
	}

	if n := len(loadCursorAccountLogins().Logins); n != 2 {
		t.Fatalf("login map has %d logins, want 2", n)
	}
	refs := map[string]string{" ide.login@example.COM": cursorAccountRef(ideTok), "agent.login@example.org": cursorAccountRef(agentTok)}
	for email, want := range refs {
		if got, ok := cursorHookAccountRef(stopPayload(t, email)); !ok || got != want {
			t.Errorf("%q: ref=%q ok=%v, want %q", email, got, ok, want)
		}
		res, ok := normalize.NormalizeCursorHook(redact.RedactBytes(stopPayload(t, email)), normalize.CursorHookOptions{})
		if !ok {
			t.Fatal("stop produced no events")
		}
		ref, _ := cursorHookAccountRef(stopPayload(t, email))
		stampCursorAccountRef(res.Events, ref)
		*evs = append(*evs, res.Events...)
	}

	banned := []string{ideTok, agentTok, "ide.login", "Ide.Login", "agent.login", "Agent.Login", "example.com", "example.org",
		"Example.com", "Example.org", subIDE, subAgent, hex.EncodeToString(state.CursorAttributionKey())}
	check := func(where string, blob []byte) {
		for _, b := range banned {
			if strings.Contains(string(blob), b) {
				t.Fatalf("%s leaked %q", where, b)
			}
		}
	}
	for _, e := range *evs {
		redact.ProjectEvent(&e, false)
		blob, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		check(e.Kind+" event", blob)
	}
	logs, _ := os.ReadFile(stderr.Name())
	check("stderr", logs)
	err = filepath.Walk(state.StateDir(), func(p string, info os.FileInfo, err error) error {
		// The install key's own file is where the key is SUPPOSED to live.
		if err != nil || info.IsDir() || filepath.Base(p) == "cursor-attribution-key" {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		check("state file "+filepath.Base(p), b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t *testing.T, path string, v interface{}) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
