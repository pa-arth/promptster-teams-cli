package capture

// cursor-vendor-usage-rail — THE GO MIRROR OF THE WIRE CONTRACT.
//
// The authority is promptster-backend `packages/contracts/src/cursorVendorUsage.ts`.
// Go cannot import it (the contract ships as a published npm package, consumed by
// the backend and the teams frontend), so these literals are mirrored BY HAND and
// pinned by `cursor_vendor_contract_test.go`. If you change a value here, change
// it there in the same change — a value present on one side and not the other is
// not a compile error on either.
//
// This file is DECLARATION ONLY. No credential read, no HTTP, no poll loop. It
// lands ahead of the collector so the CLI, backend, and frontend lanes can be
// built in parallel against one frozen vocabulary.
//
// WHY THE RAIL EXISTS. Cursor's on-device hook rail (`stop`, `afterAgentResponse`)
// never fires on the headless `cursor-agent -p` path: no IDE, no hook dispatcher,
// no `stop`. That is structural, not a configuration gap, and it is where the
// automated-agent spend lives. A machine-lane zero is by construction — which
// describes the cause, not its acceptability.

const (
	// CursorHookIntegration is the on-device hook + transcript rail. Its usage
	// rows are a per-TURN SUM across the turn's model calls, which is why the
	// backend bars them from reporting a per-request context.
	CursorHookIntegration = "cursor"

	// CursorVendorIntegration is the vendor usage rail: genuinely per-REQUEST
	// rows carrying the charge Cursor itself states.
	//
	// THE NAME IS LOAD-BEARING IN BOTH DIRECTIONS, AND THE SUBSTRING IS
	// DELIBERATE. On the backend, exact-match sets keyed on the integration id
	// EXCLUDE this value (a per-request rail must not be treated as a turn sum),
	// while substring matchers — the fluency source family, the capture
	// capability matrix, the cross-tool rollups — INCLUDE it, because this is a
	// second rail on the SAME TOOL and must roll up under Cursor everywhere a
	// family is computed. Renaming this to anything without "cursor" as a
	// substring compiles fine and silently drops the rail out of every family
	// rollup.
	//
	// It is also the emitted `source_service` (the backend derives that from
	// `source.integration` first), so it must stay registered on the Cursor row
	// of the backend's TOOL_ROSTER. An unrostered source_service is DISCARDED at
	// ingest, silently.
	CursorVendorIntegration = "cursor-vendor"

	// CursorVendorUsageScope tags how the row accumulates: it may be summed as
	// emitted rather than differenced against a running total.
	//
	// NOTE THIS ANSWERS ONLY THAT ONE QUESTION. It does NOT say "this row is one
	// request" — on the hook rail those two answers diverge, which is why the
	// per-turn axis lives at the integration id and not here.
	CursorVendorUsageScope = "request"

	// CursorVendorUsagePolicyFlag is the org policy key on GET /v1/teams/policy
	// that gates this collector.
	//
	// FAIL-CLOSED, like CaptureAssistantProse and deliberately NOT like
	// AutoUpdate (which fails OPEN so a network blip cannot strand the fleet on
	// an old binary). The asymmetry is intentional and both live in the same
	// response.
	//
	// WHY THIS RAIL NEEDS A SWITCH WHEN NO OTHER COLLECTOR DOES: every other rail
	// reads data the engineer produced; this one AUTHENTICATES AS THE ENGINEER to
	// a third party. Every other rail degrades to lost data. This one can degrade
	// to the vendor suspending the customer's engineers mid-workday — an incident
	// with a same-day recovery requirement. A self-update cycle plus a release is
	// not an incident response, and the alternative recovery path is asking the
	// customer to uninstall.
	//
	// Worst-case stop latency is the policy cache TTL, not a release.
	CursorVendorUsagePolicyFlag = "cursorVendorUsage"

	// CursorVendorJoinKey is the only attribution path this rail has into our
	// data. The vendor endpoint is ACCOUNT-SCOPED: no cwd, no repo, no workspace,
	// no session id of its own.
	//
	// Whether this id shares a namespace with the hook rail's conversation_id /
	// the transcript uuid is UNVERIFIED (gate 0.1), and it is verified per
	// SURFACE rather than as one boolean — the plausible bad outcome is that IDE
	// conversations join and `cursor-agent` runs do not, which is the worst case
	// because headless rows have no other attribution path.
	CursorVendorJoinKey = "conversationId"
)

// CursorVendorAbsenceReason says WHY a figure is missing.
//
// Every surface on this rail reports an absence WITH A REASON rather than a
// zero, a full gauge, or silence. The distinction each value preserves: a
// collector that ran and got nothing, a collector that was not ALLOWED to run,
// and a collector that never ran at all are three different facts about an
// account. Rendered as $0.00 they become one claim — about spend we did not
// measure, on precisely the population whose spend was the question.
type CursorVendorAbsenceReason string

const (
	// CursorVendorAbsenceCredentialAbsent: no Cursor credential in the local
	// application state on this device.
	CursorVendorAbsenceCredentialAbsent CursorVendorAbsenceReason = "credential_absent" // #nosec G101 -- an absence REASON, not a credential; the value never holds one

	// CursorVendorAbsenceCredentialExpired: a credential was found but is expired
	// and could not be refreshed.
	//
	// Attributed to the CREDENTIAL, never rendered as the provider declining to
	// report. A collector that discovers expiry by getting a 401 and going quiet
	// emits a gap indistinguishable from "the engineer stopped using Cursor".
	CursorVendorAbsenceCredentialExpired CursorVendorAbsenceReason = "credential_expired" // #nosec G101 -- an absence REASON, not a credential; the value never holds one

	// CursorVendorAbsencePlatformUnsupported: the collector does not support this
	// platform. v1 ships macOS only — the credential store's location and
	// protection differ elsewhere and are unverified.
	//
	// EMITTED, NEVER SILENT: an unsupported platform must not read as an engineer
	// who stopped using Cursor.
	CursorVendorAbsencePlatformUnsupported CursorVendorAbsenceReason = "platform_unsupported"

	// CursorVendorAbsenceCollectorNotPermitted: the org's kill switch is off, or
	// the policy channel could not be reached and fail-closed denied the poll.
	//
	// An unreachable channel is NOT permission. And the stop is emitted rather
	// than silent, so a backend outage does not render as a customer who stopped
	// using Cursor.
	CursorVendorAbsenceCollectorNotPermitted CursorVendorAbsenceReason = "collector_not_permitted"

	// CursorVendorAbsenceVendorReportedNone: the vendor answered and reported no
	// usable figure for this period.
	CursorVendorAbsenceVendorReportedNone CursorVendorAbsenceReason = "vendor_reported_none"

	// CursorVendorAbsenceVendorUnreachable: transport failure or non-200.
	CursorVendorAbsenceVendorUnreachable CursorVendorAbsenceReason = "vendor_unreachable"

	// CursorVendorAbsenceVendorShapeUnrecognized: the vendor returned 200 and the
	// response did not carry the fields we parse.
	//
	// THIS IS THE ONE THAT MATTERS. An undocumented endpoint changes shape
	// silently, and the failure mode is not an outage — it is a 200 with a
	// renamed field, which parses to nothing and renders as honest-looking
	// SMALLER NUMBERS. This is what the liveness monitor alerts on; a monitor
	// that only catches transport failure catches the failure that was never the
	// risk.
	CursorVendorAbsenceVendorShapeUnrecognized CursorVendorAbsenceReason = "vendor_shape_unrecognized"
)

// cursorVendorAbsenceReasons is the full vocabulary, in the contract's order.
//
// UNEXPORTED, and handed out only as a copy by CursorVendorAbsenceReasons().
// An exported slice var is mutable by any caller in the binary, and this file's
// entire purpose is that its values do NOT drift from the TypeScript authority.
// A contract a caller can rewrite at runtime is not a contract.
var cursorVendorAbsenceReasons = []CursorVendorAbsenceReason{
	CursorVendorAbsenceCredentialAbsent,
	CursorVendorAbsenceCredentialExpired,
	CursorVendorAbsencePlatformUnsupported,
	CursorVendorAbsenceCollectorNotPermitted,
	CursorVendorAbsenceVendorReportedNone,
	CursorVendorAbsenceVendorUnreachable,
	CursorVendorAbsenceVendorShapeUnrecognized,
}

// cursorVendorSupportedPlatforms lists the GOOS values the v1 collector supports.
// Anything else emits CursorVendorAbsencePlatformUnsupported rather than nothing.
// Unexported for the same reason as the vocabulary above.
var cursorVendorSupportedPlatforms = []string{"darwin"}

// CursorVendorAbsenceReasons returns the absence vocabulary in the contract's
// order. A fresh copy per call, so a caller cannot reorder or truncate the
// vocabulary other callers read.
func CursorVendorAbsenceReasons() []CursorVendorAbsenceReason {
	out := make([]CursorVendorAbsenceReason, len(cursorVendorAbsenceReasons))
	copy(out, cursorVendorAbsenceReasons)
	return out
}

// CursorVendorSupportedPlatforms returns the supported GOOS values. A fresh copy
// per call, for the same reason.
func CursorVendorSupportedPlatforms() []string {
	out := make([]string, len(cursorVendorSupportedPlatforms))
	copy(out, cursorVendorSupportedPlatforms)
	return out
}

// CursorVendorPlatformSupported reports whether the collector runs on goos.
// Callers pass runtime.GOOS; it is a parameter so the unsupported branch is
// testable on the machine that ships the supported one.
func CursorVendorPlatformSupported(goos string) bool {
	for _, p := range cursorVendorSupportedPlatforms {
		if p == goos {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The restatement protocol — VENDOR ROWS ARE MUTABLE.
//
// Mirrors §7 of the TypeScript contract (backend 559beb3e). Measured
// 2026-08-27: a conversation carrying one row at `timestamp=1787873137139`
// (inputTokens 104089, chargedCents 48.5714) carried THREE rows twenty minutes
// later — different timestamps, different counts, a different total. Cursor
// re-aggregates after the fact and nothing in the payload marks a row as
// provisional.
//
// SO `(conversationId, timestamp)` IS ROW CONTENT, NOT ROW IDENTITY, and the
// append-only self-dedup this change was originally drafted with is wrong in
// both directions: keyed tightly it double-counts a rewritten row (new
// timestamp reads as a new row), keyed loosely it pins a stale cost forever.
//
// What replaces it is a LATEST SNAPSHOT. Every cycle re-reads the whole current
// billing period, hashes the canonical row set plus the cycle's identity into a
// `snapshotId`, stages each row under `snapshotId + ordinal`, and closes with a
// completion record carrying the row count and a content digest. Two identical
// polls produce the same `snapshotId` and dedup on the backend's ordinary event
// idempotency; a rewritten, split, or deleted row produces a NEW snapshot that
// supersedes the last one whole. There is no partial update and no arithmetic
// against a prior state.
// ---------------------------------------------------------------------------

const (
	// CursorVendorUsageEventKind carries one STAGED row. A staged row is not a
	// measurement until the completion record below validates — that is the
	// whole reason the two kinds are separate.
	CursorVendorUsageEventKind = "cursorVendorUsage"

	// CursorVendorSnapshotEventKind is the atomic commit marker for a complete
	// current-billing-period reread.
	CursorVendorSnapshotEventKind = "cursorVendorSnapshot"

	// CursorVendorSnapshotStatusComplete: the period was read in full and the
	// staged rows are the period.
	CursorVendorSnapshotStatusComplete = "complete"

	// CursorVendorSnapshotStatusAbsent: nothing was measured, and the record
	// says WHY. It stages no rows and must never activate an empty snapshot over
	// the last good one — an absence is a fact about the collector, not a
	// restatement of the account to zero.
	CursorVendorSnapshotStatusAbsent = "absent"

	// CursorVendorQuotaProvider is the `quotaProvider` literal. It is the TOOL,
	// not the rail: the quota reading is account-scoped and the same figure
	// whichever rail reports it.
	CursorVendorQuotaProvider = "cursor"
)

// cursorVendorSnapshotRowFields is the emitted key set for a staged row, in the
// contract's order.
//
// FLATTENED, and the flattening is a projection requirement rather than a
// style. The vendor nests its counts under `tokenUsage`; the device's
// default-deny projector admits KEYS, and admitting `tokenUsage` would admit
// whatever arbitrary object the vendor puts there — including fields nobody has
// seen. Four named scalars carry the same numbers and nothing else.
var cursorVendorSnapshotRowFields = []string{
	"snapshotId",
	"ordinal",
	"usageScope",
	"timestamp",
	"model",
	"kind",
	"conversationId",
	"isHeadless",
	"chargedCents",
	"inputTokens",
	"outputTokens",
	"cacheReadTokens",
	"totalCents",
	"isTokenBasedCall",
	"isChargeable",
	"owningUser",
	"subscriptionProductId",
}

// cursorVendorSnapshotCompletionFields is the emitted key set for the
// completion record, in the contract's order. `quota*` and `shape*` are the
// flattened forms of the contract's nested `quota` and `shape` objects, for the
// same projection reason as the row above.
var cursorVendorSnapshotCompletionFields = []string{
	"snapshotId",
	"status",
	"capturedAt",
	"billingCycleStartsAt",
	"billingCycleResetsAt",
	"rowCount",
	"contentSha256",
	"quotaProvider",
	"quotaCycleResetsAt",
	"quotaSpendCents",
	"quotaCapCents",
	"quotaVendorStatedPercentUsed",
	"quotaAbsenceReason",
	"shapeObservedFields",
	"shapeMissingFields",
	"shapeCursorVersion",
	"shapeHttpStatus",
	"absenceReason",
}

// CursorVendorSnapshotRowFields returns the staged-row key set, as a copy.
func CursorVendorSnapshotRowFields() []string {
	out := make([]string, len(cursorVendorSnapshotRowFields))
	copy(out, cursorVendorSnapshotRowFields)
	return out
}

// CursorVendorSnapshotCompletionFields returns the completion key set, as a copy.
func CursorVendorSnapshotCompletionFields() []string {
	out := make([]string, len(cursorVendorSnapshotCompletionFields))
	copy(out, cursorVendorSnapshotCompletionFields)
	return out
}
