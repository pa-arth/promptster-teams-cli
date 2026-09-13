package capture

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/outbox"
	"github.com/pa-arth/promptster-teams-cli/internal/policy"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
)

const cursorVendorPollInterval = 15 * time.Minute

// runCursorVendorUsageCollector performs one full current-period restatement
// immediately and then at the policy-aligned 15 minute cadence. Credentials are
// intentionally acquired inside pollCursorVendorUsage, once per cycle.
func runCursorVendorUsageCollector(ctx context.Context, deviceID string, resolver *policy.Resolver) {
	pollCursorVendorUsage(deviceID, resolver, newCursorVendorClient(), time.Now().UTC())
	ticker := time.NewTicker(cursorVendorPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case capturedAt := <-ticker.C:
			pollCursorVendorUsage(deviceID, resolver, newCursorVendorClient(), capturedAt.UTC())
		}
	}
}

func pollCursorVendorUsage(deviceID string, resolver *policy.Resolver, client *cursorVendorClient, capturedAt time.Time) {
	if !resolver.CursorVendorUsage() {
		// The kill switch runs BEFORE any store is read, and covers attribution
		// too: without the map the hook stops stamping cursorAccountRef, instead
		// of stamping refs from logins read while collection was still allowed.
		forgetCursorAccountLogins()
		queueCursorVendorEvent(buildCursorVendorAbsenceEvent(deviceID, cursorAccountRefUnknown,
			CursorVendorAbsenceCollectorNotPermitted, capturedAt, time.Time{}, time.Time{}, cursorVendorShapeRecord{}))
		return
	}
	pollCursorVendorAccounts(deviceID, client, capturedAt, readCursorCredentials())
}

// pollCursorVendorAccounts collects once per DISTINCT account across every
// credential source (§2.2). The same login in the IDE and in cursor-agent is
// one account, so its snapshot is read and emitted once.
//
// Absences are per account too. A source that is present but unusable
// (expired) emits an absence naming its accountRef, unless another source
// collected that same account this cycle. An absence with no known account
// ("unknown", e.g. no IDE store at all) is emitted only when no account was
// collected. Otherwise it would say "no Cursor credential" on a cycle that
// collected one.
func pollCursorVendorAccounts(deviceID string, client *cursorVendorClient, capturedAt time.Time, sources []cursorCredentialSource) {
	usable := map[string]cursorCredential{}
	var order []string
	for _, s := range sources {
		if s.err != nil {
			continue
		}
		// BEFORE any network call, so a vendor outage does not also turn every
		// hook turn into `unreadable:` — the store WAS read; only the vendor
		// failed. Every source is recorded, so a login seen with an email in one
		// client attributes turns even when the other copy has none.
		recordCursorAccountReading(s.cred, time.Now())
		if _, dup := usable[s.cred.accountRef]; !dup {
			usable[s.cred.accountRef] = s.cred
			order = append(order, s.cred.accountRef)
		}
	}
	for _, ref := range order {
		pollCursorVendorAccount(deviceID, client, capturedAt, usable[ref])
	}
	absent := map[string]bool{}
	for _, s := range sources {
		ref := s.cred.accountRef
		if ref == "" {
			ref = cursorAccountRefUnknown
		}
		if s.err == nil || absent[ref] {
			continue
		}
		if _, collected := usable[ref]; collected || (ref == cursorAccountRefUnknown && len(usable) > 0) {
			continue
		}
		absent[ref] = true
		queueCursorVendorEvent(buildCursorVendorAbsenceEvent(deviceID, ref, cursorCredentialAbsence(s.err),
			capturedAt, time.Time{}, time.Time{}, cursorVendorShapeRecord{}))
	}
}

// pollCursorVendorAccount is one account's full current-period restatement.
func pollCursorVendorAccount(deviceID string, client *cursorVendorClient, capturedAt time.Time, cred cursorCredential) {
	emitAbsence := func(reason CursorVendorAbsenceReason, start, end time.Time, shape cursorVendorShapeRecord) {
		queueCursorVendorEvent(buildCursorVendorAbsenceEvent(deviceID, cred.accountRef, reason, capturedAt, start, end, shape))
	}
	onTeam, err := client.cursorAccountIsOnTeam(cred)
	if err != nil {
		emitAbsence(vendorAbsenceForError(err), time.Time{}, time.Time{}, cursorVendorShapeRecord{})
		return
	}
	if onTeam {
		emitAbsence(CursorVendorAbsenceCollectorNotPermitted, time.Time{}, time.Time{}, cursorVendorShapeRecord{})
		return
	}
	period, err := client.fetchCurrentPeriod(cred)
	if err != nil {
		emitAbsence(vendorAbsenceForError(err), time.Time{}, time.Time{}, cursorVendorShapeRecord{})
		return
	}
	start, end, ok := period.cycleBounds()
	if !ok {
		emitAbsence(CursorVendorAbsenceVendorShapeUnrecognized, time.Time{}, time.Time{}, cursorVendorShapeRecord{HTTPStatus: 200})
		return
	}
	rows, shape, err := collectCursorVendorRows(client, cred, start, end)
	shape.CursorVersion = cursorApplicationVersion()
	if err != nil {
		emitAbsence(vendorAbsenceForError(err), start, end, shape)
		return
	}
	quota := &cursorVendorQuotaReading{CycleResetsAt: end}
	if period.PlanUsage == nil {
		quota.AbsenceReason = CursorVendorAbsenceVendorReportedNone
	} else {
		quota.SpendCents = period.PlanUsage.TotalSpend
		quota.CapCents = period.PlanUsage.Limit
		quota.VendorStatedPercent = period.PlanUsage.TotalPercentUsed
		if quota.SpendCents == nil || quota.CapCents == nil || quota.VendorStatedPercent == nil {
			quota.AbsenceReason = CursorVendorAbsenceVendorReportedNone
		}
	}
	snapshot := buildCursorVendorSnapshot(cred.accountRef, rows, start, end, quota, shape)
	queuedAll := true
	for _, ev := range snapshot.rowEvents(deviceID) {
		queuedAll = queueCursorVendorEvent(ev) && queuedAll
	}
	queuedAll = queueCursorVendorEvent(snapshot.completionEvent(deviceID, capturedAt, shape.CursorVersion)) && queuedAll
	if queuedAll {
		recordCursorVendorCostClaims(rows)
	}
	if verboseWatch() {
		fmt.Fprintf(os.Stderr, "cursor-vendor: queued complete snapshot (%s)\n", cursorVendorRowCount(len(rows)))
	}
}

func collectCursorVendorRows(client *cursorVendorClient, cred cursorCredential, start, end time.Time) ([]cursorVendorRow, cursorVendorShapeRecord, error) {
	var all []cursorVendorRow
	observedSet := map[string]bool{}
	var expectedTotal *int64
	for page := 1; page <= cursorVendorMaxPages; page++ {
		p, observed, err := client.fetchUsagePage(cred, page)
		if err != nil {
			return nil, cursorVendorShapeRecord{ObservedFields: sortedSet(observedSet), HTTPStatus: httpStatusFor(err)}, err
		}
		for _, f := range observed {
			observedSet[f] = true
		}
		if expectedTotal == nil {
			expectedTotal = p.TotalUsageEventsCount
		}
		all = append(all, p.UsageEventsDisplay...)
		if len(p.UsageEventsDisplay) == 0 || expectedTotal != nil && int64(len(all)) >= *expectedTotal {
			break
		}
		if page == cursorVendorMaxPages {
			return nil, cursorVendorShapeRecord{ObservedFields: sortedSet(observedSet), HTTPStatus: 200}, errors.New("cursor vendor RPC: pagination incomplete")
		}
	}
	shape := cursorVendorShapeRecord{ObservedFields: sortedSet(observedSet), HTTPStatus: 200}
	shape.MissingFields = missingExpectedFields(shape.ObservedFields)
	var rows []cursorVendorRow
	for _, r := range all {
		ts, ok := epochMillisString(r.Timestamp)
		if ok && !ts.Before(start) && ts.Before(end) {
			rows = append(rows, r)
		}
	}
	return rows, shape, nil
}

func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
func httpStatusFor(err error) int {
	var h *cursorVendorHTTPError
	if errors.As(err, &h) {
		return h.status
	}
	return 0
}

// queueCursorVendorEvent is a var so tests can capture the emitted events.
var queueCursorVendorEvent = func(ev event.Event) bool {
	if err := sign.AppendEventToLocalBuffer(&ev, false); err != nil {
		fmt.Fprintf(os.Stderr, "cursor-vendor: buffer error: %v\n", err)
	}
	if err := outbox.Append(ev); err != nil {
		fmt.Fprintf(os.Stderr, "cursor-vendor: queue error (%s): %v\n", ev.Kind, err)
		return false
	}
	return true
}
