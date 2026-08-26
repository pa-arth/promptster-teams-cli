package capture

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/normalize"
	"github.com/pa-arth/promptster-teams-cli/internal/outbox"
	"github.com/pa-arth/promptster-teams-cli/internal/policy"
	"github.com/pa-arth/promptster-teams-cli/internal/redact"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

const cursorWatchInterval = 3 * time.Second

// cursorHome returns Cursor's state root.
//
// PROMPTSTER_CURSOR_HOME is OURS, not Cursor's. CODEX_HOME and
// CLAUDE_CONFIG_DIR are documented vendor overrides that the vendor itself
// honors; no such variable is documented for Cursor, so this one is named for
// the product that reads it. It exists so tests can point the watcher at a
// fixture tree without touching the developer's real ~/.cursor — do not
// advertise it to engineers as a way to relocate Cursor.
func cursorHome() string {
	if h := os.Getenv("PROMPTSTER_CURSOR_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cursor")
}

// CursorProjectsDir is where Cursor writes agent transcripts:
//
//	<home>/projects/<munged-workspace>/agent-transcripts/<uuid>/<uuid>.jsonl
//	<home>/projects/<munged-workspace>/agent-transcripts/<uuid>/subagents/<uuid>.jsonl
//
// Verified against 61 transcripts spanning 2026-02 → 2026-07 plus one live
// `cursor-agent` run, and cross-checked against an independent implementation
// (kenn-io/agentsview, internal/parser/cursor_paths.go), which additionally
// recognises a flat `agent-transcripts/<uuid>.jsonl` shape — handled below.
func CursorProjectsDir() string {
	return filepath.Join(cursorHome(), "projects")
}

// cursorWatcherState tracks the background transcript-tailing process. Mirrors
// the Claude/Codex watcher state so `status` and `doctor` can treat all three
// uniformly.
type cursorWatcherState struct {
	PID           int    `json:"pid"`
	StartedAt     string `json:"startedAt"`
	WatchDir      string `json:"watchDir,omitempty"`
	LogPath       string `json:"logPath,omitempty"`
	LastHeartbeat string `json:"lastHeartbeat,omitempty"`
	// EventsCaptured counts events parsed and queued, not delivered — delivery
	// is asynchronous (internal/outbox).
	EventsCaptured int `json:"eventsCaptured,omitempty"`
}

func cursorWatcherStatePath() string { return filepath.Join(state.StateDir(), "cursor-watcher.json") }
func cursorWatcherLogPath() string   { return filepath.Join(state.StateDir(), "cursor-watcher.log") }

func cursorWatchProgressPath() string {
	return filepath.Join(state.StateDir(), "cursor-watcher-progress.json")
}

// cursorWatchProgress persists per-transcript byte offsets and the
// workspace-match decision so each line is processed exactly once across polls
// and watcher restarts.
//
// KEYED BY PATH RELATIVE TO THE PROJECTS DIR, not by absolute path. Cursor files
// a transcript under a munged form of the workspace path, and that name is not
// stable — see cursorClassify for the two different munging behaviours observed
// on one machine. A projects-relative key still changes if Cursor re-munges, but
// the absolute path adds the home directory for nothing. The uuid in the path is
// globally unique, which is what actually collapses aliases.
type cursorWatchProgress struct {
	Offsets map[string]int64  `json:"offsets"`
	Match   map[string]string `json:"match"`
	// Roots caches the workspace root each matched transcript resolved to, so
	// the repo identity is recovered after a daemon restart without re-scanning
	// the file.
	Roots map[string]string `json:"roots"`
	// Sessions is the session id each transcript's events are emitted under.
	// USUALLY the transcript's own uuid, but not always: a transcript Cursor
	// rewrote after a `move_agent_to_*` adopts the id of the one it continues
	// (see cursor_continuation.go), and this is where that decision is kept.
	//
	// Persisting it does two things a re-derivation could not. It survives a
	// daemon restart, so one file cannot land under two ids across a bounce; and
	// it is what lets continuations CHAIN — a third transcript matching the
	// second reads the second's recorded id, which is already the first's,
	// rather than re-running detection down the chain.
	//
	// A missing entry means "never resolved", and the resolver falls back to
	// cursorSessionIDFromPath — which is exactly the pre-continuation behaviour,
	// so an older progress file needs no migration and V does not move.
	Sessions map[string]string `json:"sessions"`
	V        int               `json:"v"`
}

// cursorProgressSchemaV is the current progress-file schema version. Bump it
// when a classification-rule change invalidates cached decisions; the loader
// then drops every cached "no" once, exactly as the Claude and Codex loaders do
// (their poll loops' `case "no": continue` would otherwise never re-evaluate a
// file).
const cursorProgressSchemaV = 1

func loadCursorWatchProgress() cursorWatchProgress {
	p := cursorWatchProgress{
		Offsets:  map[string]int64{},
		Match:    map[string]string{},
		Roots:    map[string]string{},
		Sessions: map[string]string{},
	}
	data, err := os.ReadFile(cursorWatchProgressPath())
	if err != nil {
		return p
	}
	_ = json.Unmarshal(data, &p)
	if p.Offsets == nil {
		p.Offsets = map[string]int64{}
	}
	if p.Match == nil {
		p.Match = map[string]string{}
	}
	if p.Roots == nil {
		p.Roots = map[string]string{}
	}
	if p.Sessions == nil {
		p.Sessions = map[string]string{}
	}
	if p.V < cursorProgressSchemaV {
		for k, v := range p.Match {
			if v == "no" {
				delete(p.Match, k)
			}
		}
		p.V = cursorProgressSchemaV
	}
	return p
}

func saveCursorWatchProgress(p cursorWatchProgress) {
	data, err := json.Marshal(p)
	if err != nil {
		return
	}
	tmp := cursorWatchProgressPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, cursorWatchProgressPath())
}

func loadCursorWatcherState() (cursorWatcherState, error) {
	data, err := os.ReadFile(cursorWatcherStatePath())
	if err != nil {
		return cursorWatcherState{}, err
	}
	var s cursorWatcherState
	if err := json.Unmarshal(data, &s); err != nil {
		return cursorWatcherState{}, err
	}
	return s, nil
}

func saveCursorWatcherState(s cursorWatcherState) error {
	path := cursorWatcherStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func clearCursorWatcherState() {
	_ = os.Remove(cursorWatcherStatePath())
	_ = os.Remove(cursorWatchProgressPath())
}

func isCursorWatcherRunning() (cursorWatcherState, bool) {
	st, err := loadCursorWatcherState()
	if err != nil || st.PID <= 0 {
		return cursorWatcherState{}, false
	}
	if processExists(st.PID) {
		return st, true
	}
	clearCursorWatcherState()
	return cursorWatcherState{}, false
}

// RunCursorWatcher is the main loop for the `cursor-watch` subcommand. It tails
// Cursor agent transcripts whose observed working paths sit inside the watched
// workspace, normalizes each new line, redacts on-device, and queues events.
//
// It also enrolls the USER-SCOPE hook rail (~/.cursor/hooks.json) below, which
// is the one file this CLI writes for Cursor. The scope is the whole point: the
// project-local `<workspace>/.cursor/hooks.json` that Cursor's docs lead with is
// a tracked file inside the customer's repository, enrolled per-workspace, and
// this CLI does not write into customer repos or ask an engineer to enroll a
// tool once per project. See CLAUDE.md and cursor_hooks.go.
func RunCursorWatcher() error {
	session, err := loadSession()
	if err != nil {
		return fmt.Errorf("no active session: %w", err)
	}
	if session.TaskRoot == "" {
		return fmt.Errorf("session has no task root")
	}
	if st, ok := isCursorWatcherRunning(); ok && st.PID != os.Getpid() {
		return fmt.Errorf("cursor watcher already running (pid %d)", st.PID)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := saveCursorWatcherState(cursorWatcherState{
		PID: os.Getpid(), StartedAt: now, WatchDir: session.TaskRoot,
		LogPath: cursorWatcherLogPath(), LastHeartbeat: now,
	}); err != nil {
		return err
	}
	defer clearCursorWatcherState()

	if os.Getenv("PROMPTSTER_API_URL") == "" && session.ApiURL != "" {
		_ = os.Setenv("PROMPTSTER_API_URL", session.ApiURL)
	}

	workspace := resolvePath(session.TaskRoot)
	startCutoff := session.StartedAt.Add(-2 * time.Minute)

	// SIGTERM as well as SIGINT — see the matching note in RunClaudeWatcher.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	client := &http.Client{Timeout: 5 * time.Second}
	// Enroll (or repair) the user-scope Cursor hooks. Best-effort by contract:
	// a failure here leaves transcript-only capture, which is exactly the state
	// this watcher already provides. This is also the automatic migration for an
	// already-installed fleet — the daemon self-updates, re-execs, and lands here.
	EnsureCursorHooksBestEffort()

	processors := map[string]*normalize.CursorTranscriptProcessor{}
	eventsCaptured := 0
	// See pollCursorTranscripts: the first poll seeds pre-existing transcripts to
	// EOF; every later poll tails a newly-appeared transcript from 0.
	firstPoll := true

	// Org capture policy, fail-closed and refreshed off the hot path. This
	// watcher emits no assistant prose at all — a Cursor transcript carries no
	// model and this rail mints no `ai_response` (the hook rail does, but it
	// carries only a model name, never prose) — so the gate cannot change what
	// ships here. It is threaded through anyway so every capture path reaches the
	// buffer by the identical call, and a future kind cannot quietly bypass it.
	//
	// Built BEFORE the drain because the drain reads its batch-ingest capability.
	policyResolver := policy.NewResolver(session.SessionToken)

	// Process-wide singleton shared with the Claude and Codex watchers — one
	// device-wide queue, one drain. See outbox.StartDrain.
	outbox.StartDrain(client, session.SessionToken, policyResolver.BatchIngest)
	policyCtx, cancelPolicy := context.WithCancel(context.Background())
	defer cancelPolicy()
	policyResolver.StartBackground(policyCtx)

	if verboseWatch() {
		fmt.Fprintf(os.Stderr, "cursor-watcher: started, polling %s every %s (workspace=%s)\n",
			CursorProjectsDir(), cursorWatchInterval, workspace)
	}

	for {
		captureProse := policyResolver.CaptureAssistantProse()
		eventsCaptured += pollCursorTranscripts(session, workspace, startCutoff, processors, firstPoll, captureProse)
		firstPoll = false

		_ = saveCursorWatcherState(cursorWatcherState{
			PID: os.Getpid(), StartedAt: now, WatchDir: session.TaskRoot,
			LogPath:       cursorWatcherLogPath(),
			LastHeartbeat: time.Now().UTC().Format(time.RFC3339Nano), EventsCaptured: eventsCaptured,
		})

		select {
		case <-signals:
			fmt.Fprintf(os.Stderr, "cursor-watcher: shutting down (captured %d events)\n", eventsCaptured)
			return nil
		case <-time.After(cursorWatchInterval):
		}
	}
}

// pollCursorTranscripts scans for transcripts, tails matched ones from their
// stored byte offset, and queues normalized events. Returns the number queued.
func pollCursorTranscripts(
	session Session,
	workspace string,
	startCutoff time.Time,
	processors map[string]*normalize.CursorTranscriptProcessor,
	firstPoll bool,
	captureProse bool,
) int {
	progress := loadCursorWatchProgress()
	roots := workspaceMatchRoots(workspace)
	// Sessions the hook rail already covers. Its events are strictly richer
	// (model, real durations, session outcome), so where both rails can see a
	// session the hook wins and this one stands down — otherwise one prompt and
	// one file edit would be emitted twice.
	claims := loadCursorHookClaims()
	queued := 0

	for _, path := range candidateCursorTranscripts(startCutoff) {
		key := cursorProgressKey(path)
		// A CLAIMED transcript is not skipped outright — it is narrowed to the
		// events the hook rail structurally cannot produce. See
		// cursorHookBlindKinds; nil means "emit everything".
		var only map[string]bool
		hookClaimed := isCursorHookClaimed(claims, key)
		if hookClaimed {
			only = cursorHookBlindKinds
		}
		switch progress.Match[key] {
		case "no":
			continue
		case "yes":
			// proceed to tail
		default:
			result, root := cursorClassify(path, roots)
			switch result {
			case cursorMatchYes:
				progress.Match[key] = "yes"
				progress.Roots[key] = root
				// PRE-EXISTING vs NEW, without a timestamp to ask.
				//
				// Claude and Codex read a session's start time out of the
				// transcript and route on it. A Cursor transcript carries no
				// timestamp anywhere, so the discriminator is the FIRST POLL
				// instead: anything already on disk when this watcher started is
				// pre-existing and seeded to EOF; a transcript that appears on a
				// LATER poll did not exist at startup, so it is a new session and
				// is tailed from 0.
				//
				// Getting this wrong in either direction is costly and the two
				// costs are not symmetric. Seeding everything to EOF would drop
				// every new session's opening prompt — a session only becomes
				// classifiable at its first TOOL CALL, which is already several
				// records in, so the prompt that started it would be behind the
				// seek. Tailing everything from 0 would re-upload months of
				// history on the daemon's first run. The first-poll split gives
				// the right answer for both.
				if _, seen := progress.Offsets[key]; !seen {
					if !firstPoll {
						// Appeared after startup: a genuinely new session. Tail it
						// whole so its opening prompt is captured.
						progress.Offsets[key] = 0
						break
					}
					info, err := os.Stat(path)
					if err != nil {
						// Do NOT cache "yes" on a transient stat failure: a later
						// success must still get the chance to seed EOF rather
						// than tail the whole pre-existing file from 0.
						delete(progress.Match, key)
						delete(progress.Roots, key)
						continue
					}
					// ONE EXCEPTION TO "PRESENT AT FIRST POLL = PRE-EXISTING":
					// a transcript still being WRITTEN as the watcher comes up.
					// Its mtime is after the watcher's own start, which is proof
					// it is live rather than history — the one case where the
					// absence of timestamps inside the file does not leave us
					// guessing. Seeding it to EOF would seek past the opening
					// prompt of a session happening right now, which is the
					// single most valuable thing on this rail.
					//
					// The blast radius is bounded to that one live session: every
					// older transcript still seeds, so the "first run re-uploads
					// months of history" failure this split exists to prevent
					// cannot come back through here.
					//
					// GUARDED ON A NON-ZERO StartedAt, and that guard is not
					// defensive noise. A zero StartedAt makes EVERY mtime "after"
					// it, so an unset field would flip this from "tail the one
					// live session" to "tail every transcript on disk from byte
					// 0" — precisely the months-of-history replay the split
					// exists to prevent, reintroduced by a field nobody set. Fail
					// to the seeding side.
					if !session.StartedAt.IsZero() && info.ModTime().After(session.StartedAt) {
						progress.Offsets[key] = 0
						break
					}
					progress.Offsets[key] = info.Size()
				}
			case cursorMatchNo:
				progress.Match[key] = "no"
				continue
			default:
				// Undecided — the transcript has not revealed a path yet (a turn
				// that has not used a tool). Retry next poll. Caching "no" here
				// is the bug that silently dropped whole Codex sessions.
				continue
			}
		}

		proc := processors[key]
		if proc == nil {
			// NOT cursorSessionIDFromPath DIRECTLY. The filename answers "what
			// does this path say"; this answers "which conversation is this",
			// and a `move_agent_to_*` makes those two different — Cursor rewrites
			// the whole conversation under a new uuid in a new project dir, and
			// taking the new filename re-ingests every prompt and tool call so
			// far as a second session. See cursor_continuation.go.
			//
			// The per-path offset bookkeeping is untouched: `key` is still the
			// path, the new file still gets its own cursor, and only the id the
			// events carry changes.
			sessionID, cacheable := cursorResolveSessionID(path, key, progress, hookClaimed)
			if cacheable {
				progress.Sessions[key] = sessionID
			}
			proc = normalize.NewCursorTranscriptProcessor(sessionID)
			if isCursorSidechainFile(path) {
				proc.Sidechain = true
				// The child's OWN id, which cursorSessionIDFromPath deliberately
				// does not return — it rolls the child up to its parent so one
				// conversation stays one session. Both are needed and they are
				// different questions: the session id says WHICH CONVERSATION,
				// the lane id says WHICH DELEGATED AGENT. Keeping only the first
				// is why Cursor was the one rail where two subagents running at
				// once were indistinguishable.
				proc.LaneID = cursorLaneIDFromPath(path)
			} else {
				// Resolve the repo identity ONCE per transcript from the workspace
				// root this session matched, so every part (slug, host, tracked
				// bit) and the workdir describe ONE observation of ONE directory.
				root := progress.Roots[key]
				proc.Workdir = normalize.CursorSessionWorkdir(root)
				proc.RepoRoot, proc.RepoHost, proc.RepoTracked = sessionRepoIdentity(root)
			}
			processors[key] = proc
		}
		queued += tailCursorTranscript(path, key, progress, proc, session, captureProse, only)
	}

	saveCursorWatchProgress(progress)
	return queued
}

// candidateCursorTranscripts lists transcripts modified at/after the cutoff.
//
// The mtime filter caches NOTHING — a file touched later re-enters
// classification. Caching a "no" here is what dropped long, resumed and
// restart-spanning sessions forever in the Claude and Codex watchers.
func candidateCursorTranscripts(startCutoff time.Time) []string {
	var out []string
	_ = filepath.Walk(CursorProjectsDir(), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(filepath.Base(path), ".jsonl") {
			return nil
		}
		if !isCursorTranscriptPath(path) {
			return nil
		}
		if info.ModTime().Before(startCutoff) {
			return nil
		}
		out = append(out, path)
		return nil
	})
	return out
}

// isCursorTranscriptPath reports whether a .jsonl under the projects tree is an
// agent transcript rather than some other file Cursor keeps there (the tree
// also holds canvases, MCP state, terminals and agent-tool scratch files).
//
// The gate is the `agent-transcripts` path SEGMENT. Three layouts are accepted:
//
//	agent-transcripts/<uuid>.jsonl                    (flat)
//	agent-transcripts/<uuid>/<uuid>.jsonl             (the common shape)
//	agent-transcripts/<uuid>/subagents/<uuid>.jsonl   (subagent sidechain)
func isCursorTranscriptPath(path string) bool {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i, p := range parts {
		if p != "agent-transcripts" {
			continue
		}
		switch len(parts) - i - 1 {
		case 1, 2:
			return true
		case 3:
			return parts[len(parts)-2] == "subagents"
		}
		return false
	}
	return false
}

// isCursorSidechainFile reports whether a transcript is a subagent's.
func isCursorSidechainFile(path string) bool {
	return filepath.Base(filepath.Dir(path)) == "subagents"
}

// cursorSessionIDFromPath derives the OWNING session id from a transcript path,
// so a processor has an identity before it reads a line.
//
//	agent-transcripts/<uuid>.jsonl                    → the filename
//	agent-transcripts/<uuid>/<uuid>.jsonl             → the filename
//	agent-transcripts/<parent>/subagents/<child>.jsonl → the GRANDPARENT dir
//
// Rolling a subagent up to its parent is what keeps one conversation as one
// session. Taking the child's own uuid would fragment every delegated task into
// a phantom session — the exact failure that split one engineer's hour of Codex
// work into seven sessions.
func cursorSessionIDFromPath(path string) string {
	if isCursorSidechainFile(path) {
		return filepath.Base(filepath.Dir(filepath.Dir(path)))
	}
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}

// cursorLaneIDFromPath is the subagent transcript's OWN uuid — its filename.
// Empty for a main-chain transcript, which has no lane: the main thread is not a
// delegated agent.
//
// Deliberately NOT the inverse of cursorSessionIDFromPath. That function answers
// "which conversation does this belong to" and must keep returning the parent;
// this one answers "which delegated agent is this". Deriving either from the
// other is what collapsed them in the first place.
func cursorLaneIDFromPath(path string) string {
	if !isCursorSidechainFile(path) {
		return ""
	}
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}

// cursorProgressKey is a transcript's identity for offset/match bookkeeping:
// its path relative to the projects dir, slash-normalized so a key written on
// one platform reads the same on another.
func cursorProgressKey(path string) string {
	rel, err := filepath.Rel(CursorProjectsDir(), path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

type cursorMatchResult int

const (
	cursorMatchUndecided cursorMatchResult = iota
	cursorMatchYes
	cursorMatchNo
)

// cursorMaxClassifyLines bounds the scan for a workspace-revealing path. A real
// session reaches a tool call well inside this; a transcript that never does is
// a pure-prose conversation we cannot place, and it stays undecided rather than
// being cached as a mismatch.
const cursorMaxClassifyLines = 200

// cursorClassify decides whether a transcript belongs to this capture session,
// and returns the workspace root it matched.
//
// WHY IT READS THE FILE INSTEAD OF THE DIRECTORY NAME. Cursor files transcripts
// under a munged form of the workspace path, and that name cannot be trusted:
// two different munging behaviours are observable on one machine — a
// full-length name (`Users-paarthjamdagneya-repos-promptster-backend`) and a
// stem truncated to 43 characters with a 7-hex suffix
// (`Users-paarthjamdagneya-repos-promptster-bac-ed6a380`). The truncated form is
// not reversible at all, and matching forward would silently miss whichever
// form the running Cursor build does not produce. So the decision comes from
// absolute paths the agent actually touched, which are exact.
//
// Returns UNDECIDED — never NO — when no path has appeared yet.
func cursorClassify(path string, roots []string) (cursorMatchResult, string) {
	// #nosec G304 -- path is a Cursor transcript discovered under the projects dir by the watcher, not user input; opened read-only.
	f, err := os.Open(path)
	if err != nil {
		return cursorMatchUndecided, ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	scanned := 0
	sawOutsidePath := false
	for scanner.Scan() {
		scanned++
		if scanned > cursorMaxClassifyLines {
			break
		}
		observed := normalize.CursorObservedPath(scanner.Bytes())
		if observed == "" {
			continue
		}
		resolved := resolvePath(observed)
		for _, root := range roots {
			if pathWithin(resolved, root) {
				return cursorMatchYes, root
			}
		}
		sawOutsidePath = true
	}

	// ONE OUTSIDE PATH IS NOT A MISMATCH, AND TREATING IT AS ONE DROPPED WHOLE
	// SESSIONS.
	//
	// This used to return NO on the FIRST path that fell outside every root. But
	// an agent working in the watched workspace routinely touches something
	// outside it first — a global config, a doc under ~, a file in a sibling repo
	// it was asked to compare against. That single record cached a permanent
	// "no", and every prompt, edit and command the session went on to make inside
	// the workspace was skipped in silence.
	//
	// So NO now means "every path this transcript has revealed so far is outside,
	// across the whole scan window" — the whole file, not its first tool call.
	// That is still cacheable: it is the honest reading of a transcript that has
	// shown us nothing of ours.
	//
	// A file that revealed no path at all stays UNDECIDED, as before. Caching a
	// "no" on a still-growing file is what once dropped whole Codex sessions.
	//
	// Hitting the line cap with only outside paths still caches NO. It has to:
	// otherwise every long session belonging to another workspace would be
	// re-scanned on every 3s poll forever, and an engineer with several repos
	// open has plenty of those.
	if sawOutsidePath {
		return cursorMatchNo, ""
	}
	return cursorMatchUndecided, ""
}

// tailCursorTranscript reads new complete lines from the stored offset,
// normalizes them, queues the resulting events, and advances the offset. A
// trailing partial line is left for the next poll.
//
// Advancing the offset unconditionally is safe only because delivery is durable
// (internal/outbox): once queued, an event is delivered or loudly dropped, so
// the offset no longer doubles as a delivery receipt.
// cursorHookBlindKinds are the event kinds the HOOK rail cannot emit, so the
// transcript rail must keep covering them even on a session the hooks claimed.
//
// WHY THIS EXISTS. The rail handoff was whole-transcript: a claimed session was
// seeked to EOF unread, on the reasoning that the hook payload is strictly
// richer. That is true of everything the hooks REPORT — and Cursor exposes no
// hook for an MCP call or a subagent dispatch. We register sessionStart /
// sessionEnd / beforeSubmitPrompt / afterFileEdit / afterShellExecution /
// postToolUseFailure / afterAgentThought, and none of them carries either. So
// on every hook-enrolled machine — the recommended install — delegation and MCP
// identity were captured by nothing at all, and the asset boards read zero.
//
// NO DOUBLE-CAPTURE BY CONSTRUCTION, which is the property that makes this safe
// rather than a partial re-opening of the thing the claim ledger prevents: the
// two kinds here are disjoint from every kind the hook normalizer emits, so no
// event can arrive twice. Both rails also agree on session identity — the hook's
// payload sessionId equals the transcript filename stem, checked against 7 live
// claims on a hook-enrolled machine — so these land on the SAME session the
// hooks are populating rather than forking a second one.
//
// Widen this set only with the same argument: the hook rail must be structurally
// incapable of the kind, not merely currently unregistered.
var cursorHookBlindKinds = map[string]bool{
	"task_dispatch": true,
	"mcp_call":      true,
}

func tailCursorTranscript(
	path, key string,
	progress cursorWatchProgress,
	proc *normalize.CursorTranscriptProcessor,
	session Session,
	captureProse bool,
	// only limits emission to these event kinds; nil emits everything.
	only map[string]bool,
) int {
	// #nosec G304 -- path is a Cursor transcript discovered under the projects dir by the watcher, not user input; opened read-only.
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	offset := progress.Offsets[key]
	if _, err := f.Seek(offset, 0); err != nil {
		return 0
	}

	reader := bufio.NewReader(f)
	consumed := int64(0)
	queued := 0
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			break // no trailing newline yet — leave the partial line for next poll
		}
		lineOffset := offset + consumed
		consumed += int64(len(line))
		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" {
			continue
		}
		// Scrub secrets BEFORE parsing — the same pre-parse pass the Codex
		// watcher applies. A Cursor transcript carries prompt text, shell command
		// bodies and file contents the agent wrote, any of which may hold a key
		// the engineer pasted or a tool printed.
		redacted := redact.RedactBytes([]byte(trimmed))
		for _, ev := range proc.Process(redacted, lineOffset) {
			if only != nil && !only[ev.Kind] {
				continue
			}
			queued += emitCursorEvent(ev, session, captureProse)
		}
	}

	if consumed > 0 {
		progress.Offsets[key] = offset + consumed
		if queued > 0 && verboseWatch() {
			fmt.Fprintf(os.Stderr, "cursor-watcher: queued %d event(s) from %s\n", queued, filepath.Base(path))
		}
	}
	return queued
}

// emitCursorEvent stamps, dedupes, ledgers and queues one normalized event
// through the IDENTICAL path every other capture surface uses — same device
// stamping, same path relativization, same projection, same signing.
func emitCursorEvent(ev event.Event, session Session, captureProse bool) int {
	// SessionID comes from the transcript; DeviceID from the environment. Kept
	// sourced separately on purpose — collapsing them is what once made every
	// session on a machine look like one session.
	ev.DeviceID = session.DeviceID
	normalize.RelativizeEventPaths(&ev, session.TaskRoot)
	// Cursor is ALWAYS live — the false below is a fact about this surface, not
	// a default. Two independent reasons, either one sufficient: it never
	// backfills (no trustworthy session timestamp to bound a replay with, which
	// is why the 28-day history window is Claude + Codex only), and its events
	// carry the TURN'S START anchor rather than a per-record time
	// (CursorTranscriptProcessor.eventTs), so a 46-minute turn's live edits look
	// arbitrarily old. Inferring replay from age here would silently disable
	// live cross-channel dedupe and freeze the per-path write stamp that
	// durabilitySeedAuthorized and reworkSeedEvidence read as "the agent wrote
	// this again".
	const replay = false
	// Record AI bash execution windows for later commit-attribution recovery.
	// No-op unless this is an AI-attributed `command` event.
	recordAiBashWindow(&ev, session.TaskRoot, replay)
	// Cross-channel idempotency: skip a file_diff whose resulting content the
	// git watcher already emitted.
	if !dedupeFileDiff(session.TaskRoot, &ev, replay) {
		return 0
	}
	// Ledger first — it projects, scrubs and signs ev in place, so the queued
	// copy is the exact bytes to ship.
	if err := sign.AppendEventToLocalBuffer(&ev, captureProse); err != nil {
		fmt.Fprintf(os.Stderr, "cursor-watcher: buffer error: %v\n", err)
	}
	if err := outbox.Append(ev); err != nil {
		fmt.Fprintf(os.Stderr, "cursor-watcher: queue error (%s): %v\n", ev.Kind, err)
		return 0
	}
	return 1
}
