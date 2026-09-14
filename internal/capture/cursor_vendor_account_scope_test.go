package capture

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/policy"
)

const emptyUsage = `{"totalUsageEventsCount":0,"usageEventsDisplay":[]}`

// permittedCursorPolicy is a resolver that fetched an explicit, fresh
// cursorVendorUsage:true from a test policy endpoint.
func permittedCursorPolicy(t *testing.T) *policy.Resolver {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"cursorVendorUsage":true}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("PROMPTSTER_API_URL", srv.URL)
	r := policy.NewResolver("PSE-TEST")
	r.Refresh()
	if !r.CursorVendorUsage() {
		t.Fatal("precondition: policy did not permit collection")
	}
	return r
}

// fakeVendor answers the three RPCs with an identical empty period for every
// bearer and counts GetCurrentPeriodUsage calls per bearer.
func fakeVendor(t *testing.T) (*cursorVendorClient, map[string]int) {
	calls := map[string]int{}
	return &cursorVendorClient{base: cursorVendorAPIDefaultBase, http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case cursorMethodTeams:
			return response(`{}`), nil
		case cursorMethodCurrentUsage:
			calls[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]++
			return response(`{"billingCycleStart":"1785542400000","billingCycleEnd":"1788220800000"}`), nil
		case cursorMethodUsageEvents:
			return response(emptyUsage), nil
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

func vendorSnapshotData(evs []event.Event, status string) []map[string]interface{} {
	var out []map[string]interface{}
	for _, e := range evs {
		if d := e.Data.(map[string]interface{}); e.Kind == "cursorVendorSnapshot" && d["status"] == status {
			out = append(out, d)
		}
	}
	return out
}

func ideLogin(t *testing.T, sub, email string, exp int64) string {
	token := jwtWithClaims(t, fmt.Sprintf(`{"sub":%q,"exp":%d}`, sub, exp))
	t.Setenv(cursorStateDBEnv, makeCursorStateDB(t, map[string]string{
		cursorAuthAccessTokenKey: token, cursorAuthRefreshTokenKey: token, cursorAuthCachedEmailKey: email,
	}))
	return token
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
	// Zero cycle bounds, as an absence emitted before the period is known has.
	now := time.Now()
	ea := buildCursorVendorAbsenceEvent("dev", "aaaaaaaaaaaaaaaa", CursorVendorAbsenceCredentialExpired, now, time.Time{}, time.Time{}, cursorVendorShapeRecord{})
	eb := buildCursorVendorAbsenceEvent("dev", "bbbbbbbbbbbbbbbb", CursorVendorAbsenceCredentialExpired, now, time.Time{}, time.Time{}, cursorVendorShapeRecord{})
	if ea.Data.(map[string]interface{})["snapshotId"] == eb.Data.(map[string]interface{})["snapshotId"] || ea.ID == eb.ID {
		t.Fatal("two accounts' absences share an id")
	}
}

// An engineer who switches the IDE between two logins across cycles: both land
// in the login map (a hook turn from either attributes), and their identical
// empty periods produce two distinct snapshot ids.
func TestCursorVendorIDELoginSwitchAcrossCycles(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	resolver := permittedCursorPolicy(t)
	client, calls := fakeVendor(t)
	evs := captureVendorEvents(t)

	one := ideLogin(t, "auth0|login_one", "one@example.com", 4102444800)
	pollCursorVendorUsage("dev", resolver, client, time.Now())
	two := ideLogin(t, "auth0|login_two", "two@example.com", 4102444800)
	pollCursorVendorUsage("dev", resolver, client, time.Now())

	if calls[one] != 1 || calls[two] != 1 {
		t.Fatalf("collections=%v, want one per login", calls)
	}
	complete := vendorSnapshotData(*evs, CursorVendorSnapshotStatusComplete)
	if len(complete) != 2 || complete[0]["snapshotId"] == complete[1]["snapshotId"] {
		t.Fatalf("complete snapshots = %v, want 2 with distinct ids", complete)
	}
	for email, token := range map[string]string{"one@example.com": one, "two@example.com": two} {
		if ref, ok := cursorHookAccountRef(stopPayload(t, email)); !ok || ref != cursorAccountRef(token) {
			t.Errorf("%s: ref=%q ok=%v, want %q", email, ref, ok, cursorAccountRef(token))
		}
	}
}

// A malformed IDE token is an absent credential: no vendor call, no login-map
// entry, and an absence under "unknown".
func TestCursorVendorMalformedIDETokenIsAbsent(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	resolver := permittedCursorPolicy(t)
	const malformed = "not-a-jwt-ide-value"
	t.Setenv(cursorStateDBEnv, makeCursorStateDB(t, map[string]string{
		cursorAuthAccessTokenKey: malformed, cursorAuthRefreshTokenKey: malformed, cursorAuthCachedEmailKey: "ide@example.com",
	}))
	client, calls := fakeVendor(t)
	evs := captureVendorEvents(t)
	pollCursorVendorUsage("dev", resolver, client, time.Now())

	if len(calls) != 0 {
		t.Fatalf("collections=%v, want none", calls)
	}
	abs := vendorSnapshotData(*evs, CursorVendorSnapshotStatusAbsent)
	if len(abs) != 1 || abs[0]["accountRef"] != cursorAccountRefUnknown || abs[0]["absenceReason"] != string(CursorVendorAbsenceCredentialAbsent) {
		t.Fatalf("absences = %v, want one credential_absent under unknown", abs)
	}
	if _, err := os.Stat(cursorAccountReadingPath()); !os.IsNotExist(err) {
		t.Fatalf("malformed token wrote the login map: %v", err)
	}
}

// An expired IDE login is still a known account: its absence names it.
func TestCursorVendorExpiredIDELoginAbsenceNamesAccount(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	resolver := permittedCursorPolicy(t)
	token := ideLogin(t, "auth0|expired_login", "old@example.com", 1000000000)
	client, calls := fakeVendor(t)
	evs := captureVendorEvents(t)
	pollCursorVendorUsage("dev", resolver, client, time.Now())

	abs := vendorSnapshotData(*evs, CursorVendorSnapshotStatusAbsent)
	if len(calls) != 0 || len(abs) != 1 || abs[0]["accountRef"] != cursorAccountRef(token) ||
		abs[0]["absenceReason"] != string(CursorVendorAbsenceCredentialExpired) {
		t.Fatalf("calls=%v absences=%v, want one credential_expired naming the login", calls, abs)
	}
}
