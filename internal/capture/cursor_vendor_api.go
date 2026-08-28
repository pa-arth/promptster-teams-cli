package capture

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// cursor-vendor-usage-rail §1.4 — the vendor's own usage RPC, called from the
// device the credential lives on.
//
// `api2.cursor.sh` IS THIS CLI'S FIRST NON-PROMPTSTER, NON-GITHUB OUTBOUND HOST.
// Every other request this binary makes goes to the customer's own Promptster
// ingest or to GitHub for a release asset. This one authenticates as the
// engineer to a third party, and it is stated here rather than absorbed as an
// existing pattern (docs/capture-surfaces.md carries the same note where the
// egress posture is described).
//
// UNDOCUMENTED ENDPOINT, SO THE SHAPE IS PART OF THE PAYLOAD. Everything here
// was measured on the wire 2026-08-27 against a live individual-Pro account. An
// undocumented RPC changes shape silently and the failure is not an outage: it
// is a 200 whose renamed field parses to nothing and renders as usage going
// down. That is why every response is walked for the fields it ACTUALLY carried
// and that observation is emitted alongside the rows (§2.4) — the backend never
// sees this response, so a server-side monitor has no subject without it.

const (
	// cursorVendorAPIDefaultBase is the ONLY origin that may receive the bearer.
	// Tests inject an http.RoundTripper instead of changing this origin.
	cursorVendorAPIDefaultBase = "https://api2.cursor.sh"

	// The three methods this collector calls. THIS LIST IS THE BOUNDARY, and it
	// is one constant away from being wider — which is exactly why the
	// disclosure describes the OPERATIONS PERFORMED and never claims the
	// credential itself is read-only. The same bearer authorizes whatever else
	// `aiserver.v1` exposes; read-only is a property of this list, not of the
	// token.
	cursorMethodUsageEvents  = "/aiserver.v1.DashboardService/GetFilteredUsageEvents"
	cursorMethodCurrentUsage = "/aiserver.v1.DashboardService/GetCurrentPeriodUsage"
	cursorMethodTeams        = "/aiserver.v1.DashboardService/GetTeams"

	// cursorVendorHTTPTimeout bounds one RPC.
	cursorVendorHTTPTimeout = 20 * time.Second

	// cursorVendorPageSize is the page the collector asks for.
	//
	// SIZED UP DELIBERATELY. An earlier in-session probe used pageSize 15 and
	// concluded the two rails disagreed by 14x; the whole discrepancy was the
	// page truncating the result. Under-paging here does not produce an error,
	// it produces a smaller number — the failure mode this rail exists to
	// remove. Larger pages also mean FEWER requests, which is the traffic shape
	// the abuse-detection risk is actually about.
	cursorVendorPageSize = 250

	// cursorVendorMaxPages caps a runaway pagination. At 250 rows a page this is
	// 25,000 rows, an order of magnitude past a month of heavy use.
	cursorVendorMaxPages = 100

	// cursorVendorMaxBodyBytes bounds one response.
	cursorVendorMaxBodyBytes = 32 << 20
)

// cursorVendorHTTPError is a non-200. It carries the STATUS and never the body:
// an error body from an auth endpoint is exactly the string most likely to
// echo a credential back at us.
type cursorVendorHTTPError struct{ status int }

func (e *cursorVendorHTTPError) Error() string {
	return "cursor vendor RPC: HTTP " + strconv.Itoa(e.status)
}

// AbsenceReason maps transport and status failures onto the emitted vocabulary.
// A 401/403 is attributed to the CREDENTIAL rather than to the vendor declining
// to report: the two look identical downstream and only one of them has a
// remedy the engineer can act on (re-authenticate in Cursor).
func (e *cursorVendorHTTPError) AbsenceReason() CursorVendorAbsenceReason {
	if e.status == http.StatusUnauthorized || e.status == http.StatusForbidden {
		return CursorVendorAbsenceCredentialExpired
	}
	return CursorVendorAbsenceVendorUnreachable
}

// cursorVendorClient issues the three calls. It holds NO credential: the token
// is passed per call and dropped, because a client object that owns a token is
// a client object that outlives the poll cycle, and never caching the
// credential across cycles is the rule this rail turns on (§1.3).
type cursorVendorClient struct {
	http *http.Client
	base string
}

func newCursorVendorClient() *cursorVendorClient {
	return &cursorVendorClient{
		http: &http.Client{Timeout: cursorVendorHTTPTimeout},
		base: cursorVendorAPIDefaultBase,
	}
}

// validatedCursorVendorOrigin is the last gate before a Cursor bearer can be
// attached to a request. The exact-origin check is intentionally repeated per
// call: even an accidentally mutable/test-constructed client cannot redirect a
// credential. No environment override exists in production; tests preserve the
// first-party URL and inject a RoundTripper instead.
func validatedCursorVendorOrigin(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() != "api2.cursor.sh" ||
		u.Port() != "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("cursor vendor RPC: refused non-first-party origin")
	}
	return u, nil
}

// call issues one RPC and returns the raw body plus the status.
//
// THE CREDENTIAL APPEARS IN EXACTLY ONE PLACE: the Authorization header on the
// line below. It is never in the URL, never in the body, never in a returned
// error, and never in anything this function logs — which is nothing.
func (c *cursorVendorClient) call(cred cursorCredential, method string, payload any) ([]byte, int, error) {
	origin, err := validatedCursorVendorOrigin(c.base)
	if err != nil {
		return nil, 0, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequest(http.MethodPost, origin.String()+method, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+cred.token)
	req.Header.Set("Content-Type", "application/json")
	// The Connect protocol version. Without it the service answers, but this is
	// what a first-party client sends and the wire probe used it.
	req.Header.Set("Connect-Protocol-Version", "1")

	// Refuse every redirect. Origin validation above protects the initial
	// request; this protects the second request net/http would otherwise create
	// from a vendor-controlled Location header. Clone the client so the security
	// rule cannot be weakened by a caller/test mutating CheckRedirect.
	httpClient := *c.http
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// net/http wraps the request URL into transport errors, never the
		// headers — but the error is replaced anyway rather than reasoned about,
		// because "this library does not currently include the header" is the
		// kind of assumption that stops being true in a patch release.
		return nil, 0, errors.New("cursor vendor RPC: transport failure")
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, cursorVendorMaxBodyBytes))
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, &cursorVendorHTTPError{status: resp.StatusCode}
	}
	if readErr != nil {
		return nil, resp.StatusCode, errors.New("cursor vendor RPC: unreadable body")
	}
	return raw, resp.StatusCode, nil
}

// --- GetFilteredUsageEvents ---------------------------------------------------

// cursorVendorTokenUsage is the vendor's nested count object.
//
// NOTE WHAT IS NOT HERE: a cache-WRITE count. The row carries input, output and
// cache-read and nothing else. And `cacheReadTokens` is NOT a subset of
// `inputTokens` — it exceeded it on 169 of 198 sampled rows, which makes a
// subset relation arithmetically impossible and keeps this rail out of the
// backend's SUBSET_CACHE_INTEGRATIONS.
type cursorVendorTokenUsage struct {
	InputTokens     *int64   `json:"inputTokens"`
	OutputTokens    *int64   `json:"outputTokens"`
	CacheReadTokens *int64   `json:"cacheReadTokens"`
	TotalCents      *float64 `json:"totalCents"`
}

// cursorVendorRow is one line item as the vendor reports it.
//
// EVERY OPTIONAL FIELD IS A POINTER, and that is the absent-vs-zero rule in the
// type system rather than in a comment. A row whose `chargedCents` the vendor
// omitted must emit no `chargedCents`, not a 0 — a zero here is an assertion
// that a request was free, on the population whose cost was the question.
type cursorVendorRow struct {
	Timestamp             string                  `json:"timestamp"`
	Model                 string                  `json:"model"`
	Kind                  string                  `json:"kind"`
	ConversationID        string                  `json:"conversationId"`
	IsHeadless            *bool                   `json:"isHeadless"`
	ChargedCents          *float64                `json:"chargedCents"`
	TokenUsage            *cursorVendorTokenUsage `json:"tokenUsage"`
	IsTokenBasedCall      *bool                   `json:"isTokenBasedCall"`
	IsChargeable          *bool                   `json:"isChargeable"`
	OwningUser            string                  `json:"owningUser"`
	SubscriptionProductID string                  `json:"subscriptionProductId"`
}

type cursorVendorUsagePage struct {
	TotalUsageEventsCount *int64            `json:"totalUsageEventsCount"`
	UsageEventsDisplay    []cursorVendorRow `json:"usageEventsDisplay"`
}

// cursorVendorExpectedRowFields is what the collector PARSES, in the dotted form
// the shape record uses. An expected field missing from a 200 is the monitor's
// trigger; a field the vendor added that we do not parse is recorded as observed
// and is not an alarm.
var cursorVendorExpectedRowFields = []string{
	"chargedCents",
	"conversationId",
	"isHeadless",
	"kind",
	"model",
	"timestamp",
	"tokenUsage.cacheReadTokens",
	"tokenUsage.inputTokens",
	"tokenUsage.outputTokens",
	"tokenUsage.totalCents",
}

// fetchUsagePage asks for one page and returns the parsed page plus the field
// names the first row actually carried.
func (c *cursorVendorClient) fetchUsagePage(cred cursorCredential, page int) (cursorVendorUsagePage, []string, error) {
	// `teamId: 0` is what a personal account sends. `startDate`/`endDate` are
	// DELIBERATELY ABSENT: measured, the server ignores them for filtering and
	// merely drops `totalUsageEventsCount` from the response, so sending them
	// costs the completeness check and buys nothing. Bounding to the current
	// billing period is therefore CLIENT-SIDE (see collectCursorVendorSnapshot).
	raw, _, err := c.call(cred, cursorMethodUsageEvents, map[string]any{
		"teamId":   0,
		"page":     page,
		"pageSize": cursorVendorPageSize,
	})
	if err != nil {
		return cursorVendorUsagePage{}, nil, err
	}
	var parsed cursorVendorUsagePage
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return cursorVendorUsagePage{}, nil, errUnrecognizedShape
	}
	return parsed, observedUsageRowFields(raw), nil
}

// errUnrecognizedShape is a 200 we could not parse. Its own sentinel because it
// is the failure the monitor exists for and must never be collapsed into
// "unreachable".
var errUnrecognizedShape = errors.New("cursor vendor RPC: 200 with an unrecognized shape")

// observedUsageRowFields walks the RAW response for the field names the first
// line item carried, nested ones in dotted form.
//
// OFF THE RAW BYTES, NOT OFF THE PARSED STRUCT, and that is the entire point. A
// struct reports the fields we asked for; if the vendor renames `tokenUsage` the
// struct reports it absent and reports nothing about what replaced it. The
// monitor's job is to name the absent field, so the observation has to see what
// was actually there.
func observedUsageRowFields(raw []byte) []string {
	var envelope struct {
		UsageEventsDisplay []map[string]json.RawMessage `json:"usageEventsDisplay"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || len(envelope.UsageEventsDisplay) == 0 {
		return nil
	}
	seen := map[string]bool{}
	// Union across the page rather than the first row alone: the vendor omits
	// keys per row (an aborted request carries no tokenUsage), and reading one
	// row would report a field as missing on a page where it is plainly present.
	for _, row := range envelope.UsageEventsDisplay {
		for key, value := range row {
			if !isSafeShapeFieldName(key) {
				continue
			}
			seen[key] = true
			var nested map[string]json.RawMessage
			if json.Unmarshal(value, &nested) != nil {
				continue
			}
			for sub := range nested {
				if isSafeShapeFieldName(sub) {
					seen[key+"."+sub] = true
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// isSafeShapeFieldName mirrors the backend's clamp pattern for the shape
// record's string arrays: `^[A-Za-z][A-Za-z0-9.]*$`.
//
// A NAME, NEVER A VALUE — the shape record carries KEYS, and this is what keeps
// it that way even if the vendor starts returning something strange as a key.
// Anything outside the charset is dropped rather than truncated: a truncated
// name is a lie, and a truncated secret is still a secret prefix.
func isSafeShapeFieldName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	c := s[0]
	if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.':
		default:
			return false
		}
	}
	return true
}

// missingExpectedFields returns the expected names absent from observed.
func missingExpectedFields(observed []string) []string {
	present := make(map[string]bool, len(observed))
	for _, f := range observed {
		present[f] = true
	}
	var missing []string
	for _, want := range cursorVendorExpectedRowFields {
		if !present[want] {
			missing = append(missing, want)
		}
	}
	return missing
}

// --- GetCurrentPeriodUsage ----------------------------------------------------

// cursorVendorPeriod is the billing-cycle reading.
//
// A CAP, NOT A WINDOW, and the difference is not cosmetic. `billingCycleStart`
// and `billingCycleEnd` are epoch-MILLISECOND STRINGS; `planUsage` is
// denominated in CENTS.
type cursorVendorPeriod struct {
	BillingCycleStart string `json:"billingCycleStart"`
	BillingCycleEnd   string `json:"billingCycleEnd"`
	PlanUsage         *struct {
		TotalSpend *int64 `json:"totalSpend"`
		Limit      *int64 `json:"limit"`
		// THE VENDOR'S OWN PERCENTAGE, AND THE ONLY ONE THIS RAIL PUBLISHES.
		//
		// Measured 2026-08-27: totalSpend 2768 against limit 2000, with the
		// vendor stating 5.59%. The obvious derivation — spend over cap — gives
		// 138%: a gauge reading EXHAUSTED, OVER QUOTA, for an account at five
		// percent of its allowance. The real denominator appears nowhere in the
		// response. A surface that computed it the sensible way would have told a
		// customer they were over their limit while they were nowhere near it,
		// and nothing in the payload would have contradicted it.
		//
		// So this is read, never derived, and absent when the vendor states none.
		TotalPercentUsed *float64 `json:"totalPercentUsed"`
	} `json:"planUsage"`
}

func (c *cursorVendorClient) fetchCurrentPeriod(cred cursorCredential) (cursorVendorPeriod, error) {
	raw, _, err := c.call(cred, cursorMethodCurrentUsage, map[string]any{})
	if err != nil {
		return cursorVendorPeriod{}, err
	}
	var parsed cursorVendorPeriod
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return cursorVendorPeriod{}, errUnrecognizedShape
	}
	if parsed.BillingCycleStart == "" || parsed.BillingCycleEnd == "" {
		return cursorVendorPeriod{}, errUnrecognizedShape
	}
	return parsed, nil
}

// cycleBounds parses the epoch-millisecond strings into times.
func (p cursorVendorPeriod) cycleBounds() (start, end time.Time, ok bool) {
	s, errS := strconv.ParseInt(p.BillingCycleStart, 10, 64)
	e, errE := strconv.ParseInt(p.BillingCycleEnd, 10, 64)
	if errS != nil || errE != nil || s <= 0 || e <= 0 || e <= s {
		return time.Time{}, time.Time{}, false
	}
	return time.UnixMilli(s).UTC(), time.UnixMilli(e).UTC(), true
}

// --- GetTeams -----------------------------------------------------------------

// cursorVendorTeams is the account's team membership.
//
// §1.1b — THIS IS WHAT MAKES THE SCOPE BOUNDARY SELF-ENFORCING. The device
// credential rail serves non-Teams orgs ONLY: where a vendor offers an
// organization-scoped, administrator-consented API for the same data, that is
// the rail and the credential extraction never is. Written as a rule, that
// boundary depends on a human remembering it at enrollment time; asked of the
// vendor on every cycle, it enforces itself.
//
// Measured on a personal account: `{}` — the field is absent entirely, which is
// why it is a slice and an EMPTY slice means "no team". A response we cannot
// parse is NOT treated as "no team": see cursorAccountIsOnTeam.
type cursorVendorTeams struct {
	Teams []struct {
		ID   json.RawMessage `json:"id"`
		Name string          `json:"name"`
	} `json:"teams"`
}

// cursorAccountIsOnTeam reports whether this account belongs to a Cursor team.
//
// FAILS TOWARD REFUSAL. An unparseable or unreachable answer returns
// (true, err): the collector declines to run rather than assume a personal
// account. The asymmetry is deliberate — running the individual-credential rail
// on an enterprise account is the outcome the boundary exists to prevent, and it
// is not recoverable by noticing later, while declining costs one cycle of data
// that the next cycle recovers.
func (c *cursorVendorClient) cursorAccountIsOnTeam(cred cursorCredential) (bool, error) {
	raw, _, err := c.call(cred, cursorMethodTeams, map[string]any{})
	if err != nil {
		return true, err
	}
	var parsed cursorVendorTeams
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return true, errUnrecognizedShape
	}
	return len(parsed.Teams) > 0, nil
}

// vendorAbsenceForError maps any error from this file onto the emitted
// vocabulary. Nothing here may resolve to silence.
func vendorAbsenceForError(err error) CursorVendorAbsenceReason {
	if err == nil {
		return ""
	}
	if errors.Is(err, errUnrecognizedShape) {
		return CursorVendorAbsenceVendorShapeUnrecognized
	}
	var he *cursorVendorHTTPError
	if errors.As(err, &he) {
		return he.AbsenceReason()
	}
	var ce *cursorCredentialError
	if errors.As(err, &ce) {
		return ce.reason
	}
	return CursorVendorAbsenceVendorUnreachable
}

// cursorVendorRowCount renders a count for a log line that must never carry a
// row's contents. Used by the verbose watcher output only.
func cursorVendorRowCount(n int) string { return fmt.Sprintf("%d rows", n) }
