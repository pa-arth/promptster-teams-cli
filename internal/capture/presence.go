package capture

import (
	"os"
	"runtime"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
	"github.com/pa-arth/promptster-teams-cli/internal/version"
)

// Presence heartbeat.
//
// `watch` emits a tiny "presence" event on start and periodically thereafter,
// even when the developer is idle and no transcripts are being written. Its
// only job is to let the backend tell three otherwise-identical-looking states
// apart:
//
//	never onboarded   — the key exists but has NEVER sent even a heartbeat
//	onboarded, idle   — heartbeats arrive, but no qualifying tool sessions
//	active            — heartbeats AND tool sessions
//
// That distinction powers the team "seat utilization" view (a licensed seat
// that never onboards vs. one that onboarded but isn't using the tool are
// different problems). It is deliberately NOT surveillance: a presence event
// carries device + environment metadata and the list of tools being watched —
// and ZERO transcript content. See presenceData for the exact, closed shape.
//
// Identity stays anonymous: the only identifiers are the per-device hash
// (deviceID) and the team key used to authenticate the ingest request. The CLI
// never collects or sends an email or personal identity — matching a device to
// a person is done backend-side via the key, so this public repo keeps its
// "read every line that leaves the machine" guarantee.

const presenceSource = "promptster-teams"

// presenceHeartbeatInterval is how often a running `watch` re-announces
// presence. Small enough that "last seen" stays fresh for the dashboard,
// large enough to be negligible traffic.
const PresenceHeartbeatInterval = 5 * time.Minute

// presenceData is the CLOSED payload of a presence event. Every field here is
// benign environment/routing metadata — no prompts, responses, file contents,
// commands, or any other captured transcript data may ever be added. The test
// TestPresenceEventCarriesNoTranscriptContent pins this shape.
type presenceData struct {
	Device     string   `json:"device"`     // anonymous per-device hash (deviceID)
	CLIVersion string   `json:"cliVersion"` // build version of this binary
	OS         string   `json:"os"`         // runtime.GOOS
	Arch       string   `json:"arch"`       // runtime.GOARCH
	Watching   []string `json:"watching"`   // tool sources this device is watching
}

// watchedTools reports which AI tools this device is set up to capture, keyed
// by the same `source` value their events carry (so the backend can line
// presence up with activity). A tool counts as "watched" when its transcript
// directory exists on disk — i.e. the tool is installed and has run at least
// once — which is what `watch` actually tails.
func watchedTools() []string {
	tools := []string{}
	if dirExists(ClaudeProjectsDir()) {
		tools = append(tools, "claude-code")
	}
	if dirExists(codexSessionsDir()) {
		tools = append(tools, "codex")
	}
	if dirExists(ClaudeDesktopSessionsDir()) {
		tools = append(tools, "claude-desktop")
	}
	return tools
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// buildPresenceEvent constructs a presence event for the given session. It goes
// through the ordinary Event envelope so it is scrubbed, signed, and chained
// exactly like every other event (see appendEventToLocalBuffer).
//
// The payload goes through eventDataMap rather than being assigned directly:
// Data must hold a map[string]interface{} or the redaction projector
// default-denies it and the heartbeat ships with no cliVersion/device at all —
// which is exactly the fleet-health signal this event exists to carry. See
// eventDataMap.
// Presence is DEVICE-scoped, not session-scoped: it answers "is this seat
// alive", which is a property of the machine, not of any one AI-tool session.
// So its envelope sessionId stays the device id (the backend skips minting a
// session row for this kind), and data.device is read from session.DeviceID
// EXPLICITLY rather than inherited from the envelope.
//
// That independence is load-bearing. data.device backs seat utilization and
// "last seen" per device; if it ever tracked a per-session envelope id, every
// watch restart would look like a brand-new device and seat counts would
// inflate without bound.
func buildPresenceEvent(session Session) event.Event {
	e := event.NewEvent("presence", session.DeviceID)
	e.Source = presenceSource
	e.DeviceID = session.DeviceID
	e.Data = eventDataMap(presenceData{
		Device:     session.DeviceID,
		CLIVersion: version.Version,
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		Watching:   watchedTools(),
	})
	return e
}

// emitPresenceEvent builds one presence event and runs it through the SAME
// redact/sign/buffer/ingest funnel as captured events. Best-effort: a heartbeat
// that fails to send is logged only under debug and never interrupts capture.
//
// DELIBERATELY NOT QUEUED — do not "fix" this to use internal/outbox the way
// the census and the watchers do. A heartbeat is a liveness claim stamped with
// its own `ts`: retrying it three minutes later delivers "I was alive at
// 10:04", which is not durability, it is a stale assertion arriving as though
// it were news. Dropping a failed heartbeat is the CORRECT semantic — the next
// one is presenceInterval away and carries a truthful timestamp, and fleet
// health wants the latest ping, not a replay of every ping ever attempted. The
// event is still appended to the signed ledger, so the audit trail stays whole.
//
// The census is the opposite case (rare, expensive, not time-sensitive) and is
// queued; see emitConfigCensus.
func emitPresenceEvent(session Session) {
	ev := buildPresenceEvent(session)
	// captureAssistantProse=false: a presence event carries no ai_response text,
	// so the prose gate is irrelevant — pass the fail-closed default.
	if err := sign.AppendEventToLocalBuffer(&ev, false); err != nil {
		state.HookDebugf("presence buffer error: %v", err)
	}
	if err := ingest.IngestEventWithAPIKey(ev, session.SessionToken); err != nil {
		state.HookDebugf("presence send error: %v", err)
	}
}

// runPresenceHeartbeat emits presence immediately (so the very first `watch`
// run registers the device as onboarded) and then once per interval until
// stop is closed. Intended to run as a goroutine alongside the watchers.
func runPresenceHeartbeat(session Session, stop <-chan struct{}) {
	emitPresenceEvent(session)
	ticker := time.NewTicker(PresenceHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			emitPresenceEvent(session)
		}
	}
}

// startPresenceHeartbeat launches the heartbeat goroutine and returns a stop
// function the caller defers. Kept separate so runTeamsWatch stays readable.
func StartPresenceHeartbeat(session Session) (stop func()) {
	done := make(chan struct{})
	go runPresenceHeartbeat(session, done)
	return func() { close(done) }
}
