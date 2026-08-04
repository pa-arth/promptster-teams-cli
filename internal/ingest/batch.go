package ingest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxBatchResponseBytes caps the response read. One member result is ~120 bytes
// (index, id, status, maybe an error string); a 500-member batch answers in
// ~60 KB. 4 MB is generous slack while still bounding memory against a malformed
// or hostile response.
const maxBatchResponseBytes = 4 << 20

// ErrBatchUnsupported means this backend cannot answer a batch POST, so the
// caller must deliver the same events individually.
//
// It is deliberately distinct from every other error this package returns,
// because it is the only one that must NOT be retried: retrying a 404 forever
// wedges the queue against a backend that will never grow the route. The caller
// falls back instead, which is always correct — per-event delivery works
// everywhere.
var ErrBatchUnsupported = errors.New("ingest: backend does not support batch ingest")

// BatchMemberResult is one member's fate, as reported by the backend in request
// order.
//
// Status is the per-member HTTP status the backend WOULD have returned had the
// member been sent alone — 201 stored, 200 already-known (idempotent replay),
// 400 rejected, 403 refused, 500 failed. The batch request itself answers 207
// regardless; a member's fate lives here and nowhere else.
type BatchMemberResult struct {
	Index  int    `json:"index"`
	ID     string `json:"id"`
	Status int    `json:"status"`
	Error  string `json:"error"`
}

// batchResponse mirrors the 207 body.
type batchResponse struct {
	OK       bool                `json:"ok"`
	Accepted int                 `json:"accepted"`
	Rejected int                 `json:"rejected"`
	Results  []BatchMemberResult `json:"results"`
}

// IngestBatchWithClient POSTs many pre-marshalled event bodies in one request and
// returns each member's fate in request order.
//
// The member bytes are spliced into the envelope VERBATIM, exactly as
// IngestRawEventWithClient ships a single event and for the identical reason:
// the backend verifies each member's ed25519 signature by recomputing canonical
// JSON from the bytes it received, and Event.Data is an interface{}. Unmarshalling
// to build the array and re-marshalling would turn every number into a float64,
// so any value above 2^53 would re-serialize differently and fail verification —
// on a whole batch at once rather than one event. Hence the manual envelope: a
// json.Marshal of [][]byte would base64 them, and of []json.RawMessage would be
// correct but re-parse every member for no benefit.
//
// No `lane` field is sent. The backend defaults an unlabelled batch to the live
// lane, which is what every event is today; lane classification arrives with the
// outbox split (spec §1.1-§1.2) and this envelope is additive-ready for it.
func IngestBatchWithClient(
	client *http.Client,
	endpoint string,
	bodies [][]byte,
	apiKey string,
) ([]BatchMemberResult, error) {
	if len(bodies) == 0 {
		return nil, nil
	}

	var buf bytes.Buffer
	buf.WriteString(`{"events":[`)
	for i, b := range bodies {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(b)
	}
	buf.WriteString(`]}`)

	req, err := http.NewRequest(http.MethodPost, apiURL()+endpoint, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return nil, fmt.Errorf("build batch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	if pub := devicePubKeyB64(); pub != "" {
		req.Header.Set("X-Promptster-Device-Pubkey", pub)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("batch ingest request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxBatchResponseBytes))

	// A backend without the route. 501 joins 404/405 because an intermediary may
	// answer for an origin that has no such handler.
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return nil, ErrBatchUnsupported
	}

	if resp.StatusCode >= 300 {
		// Everything else — 429, 5xx, auth, and a whole-batch 400 on a malformed
		// envelope — comes back as the same typed error single delivery uses, so
		// IsRateLimited and IsIngestRejection keep working unchanged on it.
		return nil, &ingestHTTPError{
			status:     resp.StatusCode,
			body:       strings.TrimSpace(string(respBody)),
			retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	// A 2xx that is not 207 is not an answer we can act on: no per-member
	// results means no member whose delivery we can prove. Treating it as
	// unsupported sends the same events individually, which is lossless — the
	// one outcome that is never wrong here. Reachable via a proxy or a captive
	// portal answering 200 with its own body.
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, ErrBatchUnsupported
	}

	var parsed batchResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, ErrBatchUnsupported
	}
	if parsed.Results == nil {
		return nil, ErrBatchUnsupported
	}
	return parsed.Results, nil
}

// IsBatchUnsupported reports whether err means "this backend has no batch route".
func IsBatchUnsupported(err error) bool {
	return errors.Is(err, ErrBatchUnsupported)
}
