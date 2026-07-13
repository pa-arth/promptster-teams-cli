package event

import (
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- used only for RFC 4122 v5 UUID construction, not as a security primitive.
	"fmt"
	"time"
)

// newUUID generates a random UUID v4 using crypto/rand.
func NewUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant bits
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// promptsterEventNamespace is a fixed, arbitrary namespace UUID for deriving
// deterministic (v5) event ids. It never changes — the whole point is that the
// same logical source produces the same id forever, so the backend can dedupe.
var promptsterEventNamespace = [16]byte{
	0x8f, 0x2a, 0x6c, 0x1d, 0x4b, 0x77, 0x45, 0x9e,
	0xa3, 0x51, 0xd0, 0x9c, 0x2e, 0x7f, 0x88, 0x14,
}

// DeterministicUUID derives a stable RFC 4122 v5 UUID from name. Given the same
// name it always returns the same UUID, so an event whose id is derived from a
// STABLE source key (a transcript line's own `uuid`, an assistant message.id, a
// tool_use_id) gets the SAME id no matter how many times that source is
// re-read, re-tailed from a resumed/forked transcript, or resent after a
// transport error — letting the backend collapse the duplicates on ingest.
// This is the idempotency primitive that fixes CLI-side double-emission at the
// source of identity rather than relying on any single emit path being
// exactly-once.
func DeterministicUUID(name string) string {
	// #nosec G401 -- SHA-1 is mandated by RFC 4122 for v5 UUIDs; not a security use.
	h := sha1.New()
	_, _ = h.Write(promptsterEventNamespace[:])
	_, _ = h.Write([]byte(name))
	sum := h.Sum(nil)
	var b [16]byte
	copy(b[:], sum[:16])
	b[6] = (b[6] & 0x0f) | 0x50 // version 5
	b[8] = (b[8] & 0x3f) | 0x80 // variant bits
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Event is the canonical Promptster event shape sent to /v1/teams/ingest.
type Event struct {
	ID         string      `json:"id"`
	SessionID  string      `json:"sessionId"`
	Ts         string      `json:"ts"`
	Kind       string      `json:"kind"`
	Source     string      `json:"source"`
	V          int         `json:"v"`
	Data       interface{} `json:"data"`
	Actor      *Actor      `json:"actor,omitempty"`
	Provenance *Provenance `json:"provenance,omitempty"`
	RawPayload string      `json:"rawPayload,omitempty"`
	// RelatedEventIDs back-links this event to earlier events in the same
	// session (e.g. a redirect prompt to the interrupt that preceded it). The
	// backend envelope field is `relatedEventIds`. Not covered by the signing
	// message (BuildSigningMessage hashes only Data + header fields), so it is
	// pure linkage metadata — no source, no signature exposure.
	RelatedEventIDs []string `json:"relatedEventIds,omitempty"`
	// Ed25519 signature over the canonical signing message (hex). Added by
	// signAndAppendEvent during buffer append; empty on legacy unsigned events.
	Sig string `json:"sig,omitempty"`
	// Hex of the previous event's `sig` in the session chain; empty for the
	// first event in the chain or for legacy unsigned sessions.
	PrevSig string `json:"prevSig,omitempty"`
}

// Provenance captures who authored a change and how confident we are.
type Provenance struct {
	Attribution   string   `json:"attribution"`
	Confidence    float64  `json:"confidence"`
	Observability string   `json:"observability"`
	Methods       []string `json:"methods"`
}

// Actor identifies who performed the action the event records (as opposed to
// Provenance, which is about who authored a *change*). The grading pipeline
// partitions every signal by actor: only human-attributable behavior drives
// rubric tiers; agent actions are judge context.
type Actor struct {
	Type string `json:"type"`           // ai | human | system | unknown
	Role string `json:"role,omitempty"` // assistant | developer | session
}

func HumanActor() *Actor  { return &Actor{Type: "human", Role: "developer"} }
func AIActor() *Actor     { return &Actor{Type: "ai", Role: "assistant"} }
func SystemActor() *Actor { return &Actor{Type: "system", Role: "session"} }

func AIProvenance() *Provenance {
	return &Provenance{
		Attribution:   "likely_ai",
		Confidence:    0.9,
		Observability: "high",
		Methods:       []string{"hook"},
	}
}

func HumanProvenance() *Provenance {
	return &Provenance{
		Attribution:   "likely_human",
		Confidence:    0.8,
		Observability: "medium",
		Methods:       []string{"hook"},
	}
}

func NewEvent(kind, sessionID string) Event {
	if sessionID == "" {
		sessionID = "unknown"
	}
	return Event{
		ID:        NewUUID(),
		SessionID: sessionID,
		Ts:        time.Now().UTC().Format(time.RFC3339Nano),
		Kind:      kind,
		Source:    "hook",
		V:         1,
	}
}
