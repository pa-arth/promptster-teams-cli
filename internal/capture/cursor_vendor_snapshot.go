package capture

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

// cursor-vendor-usage-rail — THE LATEST-SNAPSHOT PROTOCOL.
//
// WHY THIS EXISTS, AND WHY THE OBVIOUS DESIGN IS WRONG. This rail was drafted
// with a self-dedup on `(conversationId, timestamp)`, on the assumption that the
// vendor's usage feed is an append-only ledger. It is not. Measured 2026-08-27:
// at 23:25Z one conversation carried a row `timestamp=1787873137139,
// inputTokens=104089, chargedCents=48.5714`. Twenty minutes later THAT ROW DID
// NOT EXIST — in its place, three rows with different timestamps, different
// counts and different charges, summing to a different total. Cursor
// re-aggregates after the fact and nothing in the payload marks a row as
// provisional or final.
//
// Against that, `(conversationId, timestamp)` fails in both directions at once:
// keyed tightly, a rewritten row carries a new timestamp and is counted AGAIN;
// keyed loosely, the first reading's cost is pinned forever and the correction
// never lands. Either way the number is quietly wrong, which is the failure mode
// this whole change exists to remove.
//
// SO THE UNIT OF TRUTH IS THE SNAPSHOT, NOT THE ROW.
//
//	1. Re-read the WHOLE current billing period every cycle.
//	2. Canonicalize and sort the rows deterministically, hash them together with
//	   the cycle's identity → `snapshotId`. `capturedAt` is deliberately NOT an
//	   input, so a poll that finds nothing changed produces the SAME id and
//	   dedups on the backend's ordinary event idempotency. Identical polls are
//	   free.
//	3. Stage each row as `cursorVendorUsage`, tagged `snapshotId` + `ordinal`.
//	   That pair is the ONLY ingestion identity — the row's own content never is.
//	4. Close with a `cursorVendorSnapshot` completion carrying the row count, a
//	   content digest, `capturedAt`, the billing bounds, the quota reading and
//	   the observed-shape record.
//
// A rewritten, split or deleted row therefore produces a NEW snapshot that
// SUPERSEDES the last one whole. There is no partial update, no arithmetic
// against a prior state, and no way for a correction to be read as new spend.
//
// THE MIRROR OF THIS IS `CursorVendorSnapshotRowSchema` /
// `CursorVendorSnapshotCompletionSchema` in promptster-backend
// `packages/contracts/src/cursorVendorUsage.ts`. The digest inputs below are the
// half that is NOT expressible in a zod schema, so they are written out
// exhaustively here: a backend that recomputes the digest differently rejects
// every snapshot, and a backend that does not recompute it at all is trusting a
// number for nothing.

const (
	// cursorVendorSnapshotDigestVersion prefixes every digest input.
	//
	// A VERSION ON A HASH IS NOT CEREMONY. The canonical form below is a wire
	// contract between two languages that cannot import each other; the day the
	// field order or the absent-marker changes, every snapshot's id changes with
	// no other visible symptom, and this prefix is what makes that a decision
	// rather than an accident.
	cursorVendorSnapshotDigestVersion = "cursorVendorSnapshot/v1"

	// The canonical separators. Unit separator between a row's fields, record
	// separator between rows — control characters, so no field value can contain
	// one and shift the framing.
	cursorVendorFieldSep  = "\x1f"
	cursorVendorRecordSep = "\x1e"

	// cursorVendorAbsentMarker stands for a field the vendor did not report.
	//
	// DISTINCT FROM "0", AND THAT IS THE POINT AT THE DIGEST LEVEL TOO. A row
	// whose `chargedCents` is absent and one whose `chargedCents` is 0 are
	// different facts — the first says the vendor told us nothing, the second
	// says the request was free — and a canonical form that rendered both as "0"
	// would give them the same snapshot id.
	cursorVendorAbsentMarker = "-"

	// cursorVendorCycleTimeFormat is how the billing bounds are rendered BOTH in
	// the digest and on the wire. One format, so the digest is recomputable from
	// the emitted event and not from some other rendering of the same instant.
	cursorVendorCycleTimeFormat = "2006-01-02T15:04:05.000Z"
)

// cursorVendorSnapshot is one complete reading of the current billing period.
type cursorVendorSnapshot struct {
	// SnapshotID is sha256 over the digest version, the cycle bounds, the row
	// count and every canonical row line.
	SnapshotID string
	// ContentSha256 is sha256 over the canonical row lines ALONE.
	//
	// TWO DIGESTS, DELIBERATELY, and they answer different questions. SnapshotID
	// is IDENTITY — it includes the cycle, so the same rows read in two
	// different billing cycles are two different snapshots. ContentSha256 is
	// INTEGRITY — it is what the completion record validates the staged rows
	// against, and it must not move when only the cycle framing does. Collapsing
	// them into one value would make the completion unable to distinguish "these
	// are the wrong rows" from "this is a different cycle".
	ContentSha256 string
	Rows          []cursorVendorRow
	CycleStart    time.Time
	CycleEnd      time.Time
	// Quota is the billing-cycle reading, absent when the vendor reported none.
	Quota *cursorVendorQuotaReading
	// Shape is what the vendor's response actually carried.
	Shape cursorVendorShapeRecord
}

// cursorVendorQuotaReading is the flattened quota reading.
//
// NOTE WHAT IS ABSENT: any percentage we computed. There is a vendor-stated one
// and nothing else. See cursorVendorPeriod.PlanUsage.TotalPercentUsed for the
// measurement that settled it — the sensible derivation read 138% on an account
// the vendor put at 5.59%.
type cursorVendorQuotaReading struct {
	CycleResetsAt       time.Time
	SpendCents          *int64
	CapCents            *int64
	VendorStatedPercent *float64
	AbsenceReason       CursorVendorAbsenceReason
}

// cursorVendorShapeRecord is §2.4's observed-shape record.
type cursorVendorShapeRecord struct {
	ObservedFields []string
	MissingFields  []string
	CursorVersion  string
	HTTPStatus     int
}

// canonicalRowLine renders one row into its digest form.
//
// FIELD ORDER IS THE CONTRACT'S ORDER and must not be "tidied". Every optional
// value is either its canonical rendering or the absent marker; nothing is
// defaulted.
func canonicalRowLine(r cursorVendorRow) string {
	fields := []string{
		r.Timestamp,
		r.Model,
		r.Kind,
		r.ConversationID,
		boolKey(r.IsHeadless),
		floatKey(r.ChargedCents),
		intKey(tokenInput(r)),
		intKey(tokenOutput(r)),
		intKey(tokenCacheRead(r)),
		floatKey(tokenTotalCents(r)),
		boolKey(r.IsTokenBasedCall),
		boolKey(r.IsChargeable),
		emittableOwningUser(r.OwningUser),
		r.SubscriptionProductID,
	}
	return strings.Join(fields, cursorVendorFieldSep)
}

func tokenInput(r cursorVendorRow) *int64 {
	if r.TokenUsage == nil {
		return nil
	}
	return r.TokenUsage.InputTokens
}

func tokenOutput(r cursorVendorRow) *int64 {
	if r.TokenUsage == nil {
		return nil
	}
	return r.TokenUsage.OutputTokens
}

func tokenCacheRead(r cursorVendorRow) *int64 {
	if r.TokenUsage == nil {
		return nil
	}
	return r.TokenUsage.CacheReadTokens
}

func tokenTotalCents(r cursorVendorRow) *float64 {
	if r.TokenUsage == nil {
		return nil
	}
	return r.TokenUsage.TotalCents
}

func intKey(v *int64) string {
	if v == nil {
		return cursorVendorAbsentMarker
	}
	return strconv.FormatInt(*v, 10)
}

// floatKey renders a float in the shortest form that round-trips.
//
// 'g' WITH PRECISION -1, NOT A FIXED NUMBER OF DECIMALS. The vendor's charges
// arrive as 35.6402 and 48.5714; rounding them for the digest would make two
// genuinely different snapshots hash the same, which is the one thing a
// canonical form may never do.
func floatKey(v *float64) string {
	if v == nil {
		return cursorVendorAbsentMarker
	}
	return strconv.FormatFloat(*v, 'g', -1, 64)
}

func boolKey(v *bool) string {
	if v == nil {
		return cursorVendorAbsentMarker
	}
	if *v {
		return "true"
	}
	return "false"
}

// emittableOwningUser drops an `owningUser` that looks like an email address.
//
// MEASURED: it is an opaque numeric string ("359430439"), and the neighbouring
// store key `cursorAuth/cachedEmail` is the reason to be careful anyway. This
// CLI's README makes a flat promise — "the CLI never collects or sends your
// email" — and a vendor field is exactly the kind of place that promise gets
// broken without anyone editing the sentence. So the shape is checked at the
// emit boundary rather than assumed from one observation.
func emittableOwningUser(s string) string {
	if strings.Contains(s, "@") {
		return ""
	}
	return s
}

// buildCursorVendorSnapshot canonicalizes, sorts and hashes the period's rows.
//
// THE SORT IS PART OF THE DIGEST CONTRACT. The vendor returns rows newest-first
// and paginates; page boundaries and re-aggregation both reorder them, and an
// order-sensitive digest would mint a new snapshot for a reordering that changed
// nothing. Sorting on the FULL canonical line (not just the timestamp) is what
// makes it total: two rows sharing a timestamp and a conversation are ordered by
// their content, so the order is a function of the SET.
func buildCursorVendorSnapshot(rows []cursorVendorRow, cycleStart, cycleEnd time.Time,
	quota *cursorVendorQuotaReading, shape cursorVendorShapeRecord) cursorVendorSnapshot {

	lines := make([]string, 0, len(rows))
	byLine := make(map[string][]cursorVendorRow, len(rows))
	for _, r := range rows {
		line := canonicalRowLine(r)
		lines = append(lines, line)
		byLine[line] = append(byLine[line], r)
	}
	sort.Strings(lines)

	// Re-materialize the rows in canonical order, so a row's emitted `ordinal`
	// is its position in the SAME order the digest was taken over. Without this
	// the ordinals would be pagination order and the completion's row count
	// would be the only thing tying the two together.
	ordered := make([]cursorVendorRow, 0, len(rows))
	consumed := map[string]int{}
	for _, line := range lines {
		idx := consumed[line]
		consumed[line] = idx + 1
		ordered = append(ordered, byLine[line][idx])
	}

	content := strings.Join(lines, cursorVendorRecordSep)
	contentSum := sha256.Sum256([]byte(content))

	identity := strings.Join([]string{
		cursorVendorSnapshotDigestVersion,
		cycleStart.UTC().Format(cursorVendorCycleTimeFormat),
		cycleEnd.UTC().Format(cursorVendorCycleTimeFormat),
		strconv.Itoa(len(lines)),
		content,
	}, cursorVendorRecordSep)
	identitySum := sha256.Sum256([]byte(identity))

	return cursorVendorSnapshot{
		SnapshotID:    hex.EncodeToString(identitySum[:]),
		ContentSha256: hex.EncodeToString(contentSum[:]),
		Rows:          ordered,
		CycleStart:    cycleStart,
		CycleEnd:      cycleEnd,
		Quota:         quota,
		Shape:         shape,
	}
}

// rowEvents builds the staged `cursorVendorUsage` events.
//
// SESSION ID IS THE CONVERSATION ID, and that single assignment is the entire
// attribution story for this rail. The vendor endpoint is account-scoped — no
// cwd, no repo, no workspace, no session of its own — so `conversationId` is its
// only path into our data. Gate zero verified on the HEADLESS surface
// (2026-08-27) that the vendor's `conversationId` is byte-identical to the
// transcript uuid the Cursor watcher already keys on, which is the half the
// change rests on: an IDE-only join would have delivered dedup for data we
// already have and zero dollars on the agent lane.
//
// A row with no conversation id falls back to the device, so its cost still
// reaches the account total rather than being dropped for lacking a join.
func (s cursorVendorSnapshot) rowEvents(deviceID string) []event.Event {
	out := make([]event.Event, 0, len(s.Rows))
	for ordinal, r := range s.Rows {
		sessionID := r.ConversationID
		if sessionID == "" {
			sessionID = deviceID
		}
		e := event.NewEvent("cursorVendorUsage", sessionID)
		e.Source = CursorVendorIntegration
		e.Actor = event.AIActor()
		e.DeviceID = deviceID
		if ts, ok := epochMillisString(r.Timestamp); ok {
			e.Ts = ts.UTC().Format(time.RFC3339Nano)
		}

		data := map[string]interface{}{
			"snapshotId": s.SnapshotID,
			"ordinal":    ordinal,
			// The accumulation tag. It says the row may be SUMMED as emitted
			// rather than differenced against a running total — and nothing else.
			// It does not say "this row is one request": on the hook rail those
			// two answers diverge, which is why the per-turn axis lives at the
			// integration id.
			"usageScope": CursorVendorUsageScope,
		}
		putStr(data, "timestamp", r.Timestamp)
		putStr(data, "model", r.Model)
		putStr(data, "kind", r.Kind)
		putStr(data, "conversationId", r.ConversationID)
		putBool(data, "isHeadless", r.IsHeadless)
		putFloat(data, "chargedCents", r.ChargedCents)
		putInt(data, "inputTokens", tokenInput(r))
		putInt(data, "outputTokens", tokenOutput(r))
		putInt(data, "cacheReadTokens", tokenCacheRead(r))
		putFloat(data, "totalCents", tokenTotalCents(r))
		putBool(data, "isTokenBasedCall", r.IsTokenBasedCall)
		putBool(data, "isChargeable", r.IsChargeable)
		putStr(data, "owningUser", emittableOwningUser(r.OwningUser))
		putStr(data, "subscriptionProductId", r.SubscriptionProductID)
		e.Data = data

		// snapshotId + ordinal, and NOTHING from the row. The row's own content
		// is explicitly not identity here — that is the defect the whole
		// protocol replaces. Two identical polls yield the same snapshotId,
		// therefore the same ids, and collapse on ingest for free.
		e.ID = event.DeterministicUUID("cursorVendorUsage:" + s.SnapshotID + ":" + strconv.Itoa(ordinal))
		out = append(out, e)
	}
	return out
}

// completionEvent builds the `cursorVendorSnapshot` commit marker.
//
// EMITTED AFTER THE ROWS, ALWAYS. A consumer must not publish staged rows until
// this validates their count and digest — a snapshot interrupted halfway is a
// partial period, and a partial period published as a total is the
// floor-presented-as-a-total defect this change exists to fix, reintroduced by
// its own transport.
func (s cursorVendorSnapshot) completionEvent(deviceID string, capturedAt time.Time, cursorVersion string) event.Event {
	e := event.NewEvent("cursorVendorSnapshot", deviceID)
	e.Source = CursorVendorIntegration
	e.Actor = event.SystemActor()
	e.DeviceID = deviceID

	data := map[string]interface{}{
		"snapshotId":           s.SnapshotID,
		"status":               CursorVendorSnapshotStatusComplete,
		"capturedAt":           capturedAt.UTC().Format(cursorVendorCycleTimeFormat),
		"billingCycleStartsAt": s.CycleStart.UTC().Format(cursorVendorCycleTimeFormat),
		"billingCycleResetsAt": s.CycleEnd.UTC().Format(cursorVendorCycleTimeFormat),
		"rowCount":             len(s.Rows),
		"contentSha256":        s.ContentSha256,
	}
	applyQuotaFields(data, s.Quota)
	applyShapeFields(data, s.Shape, cursorVersion)
	e.Data = data

	// capturedAt is IN the event id and OUT of the snapshot id, and the
	// asymmetry is the protocol. Out of the snapshot id, an unchanged poll
	// reuses the id and dedups. In the event id, a re-confirmation of the same
	// snapshot is still a distinct observation the backend can see arrive —
	// which is how "the collector is alive and the period has not changed" stays
	// distinguishable from "the collector stopped".
	e.ID = event.DeterministicUUID("cursorVendorSnapshot:" + s.SnapshotID + ":" +
		capturedAt.UTC().Format(time.RFC3339))
	return e
}

// buildCursorVendorAbsenceEvent records that the collector RAN and measured
// nothing, and why.
//
// THIS IS THE EVENT THAT MAKES THE WHOLE RAIL HONEST, and it is emitted on every
// failure path including the ones that are our fault. A collector that goes
// quiet when the kill switch flips, when the credential rotates out, or when the
// platform is unsupported produces a gap byte-identical to an engineer who
// stopped using Cursor — the absence-vs-zero failure this change exists to fix,
// reintroduced by its own safety mechanisms.
//
// It stages NO rows and its status is `absent`, so a consumer must never let it
// supersede the last good snapshot. An absence is a fact about the collector,
// never a restatement of the account to zero.
func buildCursorVendorAbsenceEvent(deviceID string, reason CursorVendorAbsenceReason,
	capturedAt time.Time, cycleStart, cycleEnd time.Time, shape cursorVendorShapeRecord) event.Event {

	e := event.NewEvent("cursorVendorSnapshot", deviceID)
	e.Source = CursorVendorIntegration
	e.Actor = event.SystemActor()
	e.DeviceID = deviceID

	// An absence still carries the cycle it is an absence FOR. Where the cycle
	// itself could not be read (the quota call is the thing that failed), the
	// capture instant stands in for both bounds: a reader can tell "we know the
	// cycle and measured nothing in it" from "we could not even establish the
	// cycle", and the completion schema still validates.
	if cycleStart.IsZero() {
		cycleStart = capturedAt
	}
	if cycleEnd.IsZero() {
		cycleEnd = capturedAt
	}
	// An empty snapshot's content digest is the digest of the empty row set —
	// computed, never hardcoded, so it stays correct if the canonical form moves.
	empty := sha256.Sum256([]byte(""))

	data := map[string]interface{}{
		"snapshotId":           cursorVendorAbsenceSnapshotID(deviceID, reason, cycleStart, cycleEnd),
		"status":               CursorVendorSnapshotStatusAbsent,
		"capturedAt":           capturedAt.UTC().Format(cursorVendorCycleTimeFormat),
		"billingCycleStartsAt": cycleStart.UTC().Format(cursorVendorCycleTimeFormat),
		"billingCycleResetsAt": cycleEnd.UTC().Format(cursorVendorCycleTimeFormat),
		"rowCount":             0,
		"contentSha256":        hex.EncodeToString(empty[:]),
		"absenceReason":        string(reason),
	}
	applyShapeFields(data, shape, shape.CursorVersion)
	e.Data = data
	e.ID = event.DeterministicUUID("cursorVendorSnapshotAbsent:" + deviceID + ":" +
		string(reason) + ":" + capturedAt.UTC().Format(time.RFC3339))
	return e
}

// cursorVendorAbsenceSnapshotID gives an absence a well-formed snapshot id.
//
// The completion schema requires a 64-hex digest in this field, and an absence
// has no rows to hash — so it hashes what it DOES know. It is deliberately a
// function of the reason and the cycle rather than of the instant, so repeated
// absences for the same reason in the same cycle carry one id and read as one
// ongoing condition rather than as a stream of distinct incidents.
func cursorVendorAbsenceSnapshotID(deviceID string, reason CursorVendorAbsenceReason,
	cycleStart, cycleEnd time.Time) string {
	// lgtm[go/weak-sensitive-data-hashing] SHA-256 is a protocol digest/id, never password storage.
	sum := sha256.Sum256([]byte(strings.Join([]string{
		cursorVendorSnapshotDigestVersion,
		"absent",
		deviceID,
		string(reason),
		cycleStart.UTC().Format(cursorVendorCycleTimeFormat),
		cycleEnd.UTC().Format(cursorVendorCycleTimeFormat),
	}, cursorVendorRecordSep)))
	return hex.EncodeToString(sum[:])
}

// applyQuotaFields flattens the quota reading onto the completion's data.
//
// FLATTENED WITH A `quota` PREFIX rather than nested, because the device's
// default-deny projector allowlists KEYS: a nested object rides through whole
// once its key is admitted, and the whole point of that projector is that no key
// carries an arbitrary payload. Same reason the row's `tokenUsage` is flattened.
//
// AN UNREPORTED FIGURE IS OMITTED, NEVER DEFAULTED. An omitted percentage
// renders as pending; a zero renders as unused and a full gauge renders as
// exhausted, and both are assertions about an account nobody measured.
func applyQuotaFields(data map[string]interface{}, q *cursorVendorQuotaReading) {
	if q == nil {
		return
	}
	data["quotaProvider"] = CursorVendorQuotaProvider
	if !q.CycleResetsAt.IsZero() {
		data["quotaCycleResetsAt"] = q.CycleResetsAt.UTC().Format(cursorVendorCycleTimeFormat)
	}
	putInt(data, "quotaSpendCents", q.SpendCents)
	putInt(data, "quotaCapCents", q.CapCents)
	putFloat(data, "quotaVendorStatedPercentUsed", q.VendorStatedPercent)
	if q.AbsenceReason != "" {
		data["quotaAbsenceReason"] = string(q.AbsenceReason)
	}
}

// applyShapeFields flattens the observed-shape record, for the same projection
// reason as the quota above.
func applyShapeFields(data map[string]interface{}, s cursorVendorShapeRecord, cursorVersion string) {
	// Complete snapshots REQUIRE both arrays even when empty. Emitting [] is an
	// observation; omitting the field is an unrecognized/legacy shape.
	data["shapeObservedFields"] = toInterfaceSlice(s.ObservedFields)
	data["shapeMissingFields"] = toInterfaceSlice(s.MissingFields)
	if cursorVersion != "" {
		data["shapeCursorVersion"] = cursorVersion
	}
	if s.HTTPStatus != 0 {
		data["shapeHttpStatus"] = s.HTTPStatus
	}
}

func toInterfaceSlice(in []string) []interface{} {
	out := make([]interface{}, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	return out
}

func putStr(data map[string]interface{}, key, value string) {
	if value != "" {
		data[key] = value
	}
}

func putInt(data map[string]interface{}, key string, v *int64) {
	if v != nil {
		data[key] = *v
	}
}

func putFloat(data map[string]interface{}, key string, v *float64) {
	if v != nil {
		data[key] = *v
	}
}

func putBool(data map[string]interface{}, key string, v *bool) {
	if v != nil {
		data[key] = *v
	}
}

// epochMillisString parses the vendor's millisecond timestamp string.
func epochMillisString(s string) (time.Time, bool) {
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil || ms <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(ms).UTC(), true
}
