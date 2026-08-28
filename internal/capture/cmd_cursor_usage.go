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
	emitAbsence := func(reason CursorVendorAbsenceReason, start, end time.Time, shape cursorVendorShapeRecord) {
		queueCursorVendorEvent(buildCursorVendorAbsenceEvent(deviceID, reason, capturedAt, start, end, shape))
	}
	if !resolver.CursorVendorUsage() {
		emitAbsence(CursorVendorAbsenceCollectorNotPermitted, time.Time{}, time.Time{}, cursorVendorShapeRecord{})
		return
	}
	cred, err := readCursorCredential()
	if err != nil {
		emitAbsence(cursorCredentialAbsence(err), time.Time{}, time.Time{}, cursorVendorShapeRecord{})
		return
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
	snapshot := buildCursorVendorSnapshot(rows, start, end, quota, shape)
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

func queueCursorVendorEvent(ev event.Event) bool {
	if err := sign.AppendEventToLocalBuffer(&ev, false); err != nil {
		fmt.Fprintf(os.Stderr, "cursor-vendor: buffer error: %v\n", err)
	}
	if err := outbox.Append(ev); err != nil {
		fmt.Fprintf(os.Stderr, "cursor-vendor: queue error (%s): %v\n", ev.Kind, err)
		return false
	}
	return true
}
