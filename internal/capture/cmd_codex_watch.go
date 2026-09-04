package capture

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
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

const codexWatchInterval = 3 * time.Second

// codexWatchMaxBytesPerPoll bounds how many rollout bytes ONE poll reads across
// all matched rollouts combined. Mirrors claudeWatchMaxBytesPerPoll — see that
// comment for why a bounded poll is what keeps shutdown responsive, keeps
// backfill progress durable across a SIGKILL, and keeps a first-boot burst from
// racing the outbox to the cap where Append drops. A var so a test can lower it.
var codexWatchMaxBytesPerPoll int64 = 8 << 20

// codexHome returns the codex state dir (CODEX_HOME or ~/.codex), where session
// rollout files live under sessions/YYYY/MM/DD/rollout-*.jsonl.
func codexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

// codexRolloutSessionID matches the uuid Codex tails onto a rollout filename:
// rollout-2026-06-11T11-24-52-019eb780-3081-7ce0-9ba0-8a0bad13b532.jsonl. The
// leading timestamp also contains dashes, so anchor on the uuid shape at the
// end rather than splitting on "-".
var codexRolloutSessionID = regexp.MustCompile(`([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\.jsonl$`)

// codexSessionIDFromPath derives the rollout's THREAD uuid from its filename,
// so a processor has an id before reading a line. It equals
// session_meta.payload.id (verified).
//
// It is a SEED, not the answer: one rollout file is one Codex thread, and since
// 0.145 one conversation spans several threads (the user's, plus one per
// subagent it delegates to). The filename cannot tell them apart — only
// session_meta carries the conversation root — so normalize.CodexConversationID
// overrides this on line 1 whenever the rollout names a root other than its own
// thread. Trusting the filename alone is what fragmented one hour of one
// engineer's work into a session of prompts and six sessions of orphaned work.
func codexSessionIDFromPath(path string) string {
	if m := codexRolloutSessionID.FindStringSubmatch(filepath.Base(path)); m != nil {
		return m[1]
	}
	return ""
}

func codexSessionsDir() string {
	return filepath.Join(codexHome(), "sessions")
}

// codexWatcherState tracks the background rollout-tailing process.
type codexWatcherState struct {
	PID       int    `json:"pid"`
	StartedAt string `json:"startedAt"`
	// WatchDir is the workspace this watcher is scoped to — see the field of the
	// same name on claudeWatcherState for why it is recorded.
	WatchDir      string `json:"watchDir,omitempty"`
	LogPath       string `json:"logPath,omitempty"`
	LastHeartbeat string `json:"lastHeartbeat,omitempty"`
	// EventsCaptured counts events parsed and queued, not delivered — delivery
	// is asynchronous (internal/outbox). In-process counter, re-derived from
	// zero each run, so renaming the tag breaks no compatibility.
	EventsCaptured int `json:"eventsCaptured,omitempty"`
}

func codexWatcherStatePath() string { return filepath.Join(state.StateDir(), "codex-watcher.json") }
func codexWatcherLogPath() string   { return filepath.Join(state.StateDir(), "codex-watcher.log") }

// codexWatchProgress persists per-rollout-file byte offsets and the
// workspace-match decision so each line is processed exactly once across polls.
type codexWatchProgress struct {
	Offsets map[string]int64 `json:"offsets"`
	// Discarding marks offsets that are inside an unsupported oversized record.
	// Persisting it prevents a restart from parsing the malformed suffix.
	Discarding         map[string]bool  `json:"discarding,omitempty"`
	ClassifyOffsets    map[string]int64 `json:"classify_offsets,omitempty"`
	ClassifyDiscarding map[string]bool  `json:"classify_discarding,omitempty"`
	ClassifyScanned    map[string]int   `json:"classify_scanned,omitempty"`
	// Match: path -> "yes"|"no" classification cache so we only read+parse a
	// file's session_meta header once.
	Match map[string]string `json:"match"`
	// V is the progress-file schema version. Bumped when a change to the
	// classification rules invalidates cached decisions; loadCodexWatchProgress
	// runs a one-time migration when the stored V is behind codexProgressSchemaV.
	V int `json:"v"`
	// RootsFP fingerprints the match ROOT SET the cached decisions were made
	// against; a change drops every cached decision so both a widened and a
	// narrowed set are applied to files already classified. Mirrors
	// claudeWatchProgress.RootsFP.
	RootsFP string `json:"roots_fp"`
}

func codexWatchProgressPath() string {
	return filepath.Join(state.StateDir(), "codex-watcher-progress.json")
}

// clearCodexClassifyState is the Codex half of clearClaudeClassifyState: the
// cursor, its skip count and its discard flag are retired together, whether the
// decision settled or the pass was abandoned. A cursor left parked past the
// header that already decided the file resumes with a partly-spent skip budget,
// which is how a transient failure turns into a permanent codexMatchNo.
func clearCodexClassifyState(p codexWatchProgress, key string) {
	delete(p.ClassifyOffsets, key)
	delete(p.ClassifyDiscarding, key)
	delete(p.ClassifyScanned, key)
}

func loadCodexWatchProgress() codexWatchProgress {
	p := codexWatchProgress{
		Offsets: map[string]int64{}, Discarding: map[string]bool{}, Match: map[string]string{},
		ClassifyOffsets: map[string]int64{}, ClassifyDiscarding: map[string]bool{}, ClassifyScanned: map[string]int{},
	}
	data, err := os.ReadFile(codexWatchProgressPath())
	if err != nil {
		// Nothing on disk means nothing to migrate: stamp the CURRENT schema so
		// the first save doesn't write v0 and make the next load re-run the
		// one-time migration on a fresh install (which drops the whole mismatch
		// cache for no reason, and would mask a broken RootsFP check by
		// rescanning anyway).
		reportProgressFileFault("codex", codexWatchProgressPath(), err, nil)
		p.V = codexProgressSchemaV
		return p
	}
	parseErr := json.Unmarshal(data, &p)
	reportProgressFileFault("codex", codexWatchProgressPath(), nil, parseErr)
	if p.Offsets == nil {
		p.Offsets = map[string]int64{}
	}
	if p.Match == nil {
		p.Match = map[string]string{}
	}
	if p.Discarding == nil {
		p.Discarding = map[string]bool{}
	}
	if p.ClassifyOffsets == nil {
		p.ClassifyOffsets = map[string]int64{}
	}
	if p.ClassifyDiscarding == nil {
		p.ClassifyDiscarding = map[string]bool{}
	}
	if p.ClassifyScanned == nil {
		p.ClassifyScanned = map[string]int{}
	}
	// Announce BEFORE migrating — same terms as the Claude loader.
	announceProgressReplay("codex", codexProgressMigrations, p.V)
	// v1: drop cached "no" decisions written by the old timestamp gate.
	if p.V < 1 {
		for k, v := range p.Match {
			if v == "no" {
				delete(p.Match, k)
			}
		}
	}
	// v2: reset old matched offsets so classification can replay only the new
	// bounded window. Preserve genuine cwd mismatches from v1.
	if p.V < 2 {
		p.Offsets = map[string]int64{}
		p.Discarding = map[string]bool{}
		p.ClassifyOffsets = map[string]int64{}
		p.ClassifyDiscarding = map[string]bool{}
		p.ClassifyScanned = map[string]int{}
		for k, v := range p.Match {
			if v == "yes" {
				delete(p.Match, k)
			}
		}
	}
	p.V = codexProgressSchemaV
	return p
}

// saveCodexWatchProgress mirrors saveClaudeWatchProgress, including why it
// reports rather than returns an error. See that function.
func saveCodexWatchProgress(p codexWatchProgress) {
	path := codexWatchProgressPath()
	data, err := json.Marshal(p)
	if err != nil {
		reportProgressWriteFault("codex", path, "cannot be SERIALISED", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		reportProgressWriteFault("codex", path, "cannot be WRITTEN", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		reportProgressWriteFault("codex", path, "cannot be COMMITTED (rename failed)", err)
		return
	}
	markProgressWriteFault("codex", false)
}

func loadCodexWatcherState() (codexWatcherState, error) {
	data, err := os.ReadFile(codexWatcherStatePath())
	if err != nil {
		return codexWatcherState{}, err
	}
	var s codexWatcherState
	if err := json.Unmarshal(data, &s); err != nil {
		return codexWatcherState{}, err
	}
	return s, nil
}

func saveCodexWatcherState(s codexWatcherState) error {
	path := codexWatcherStatePath()
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

// clearCodexWatcherState drops only the watcher's liveness state; the durable
// rollout offsets survive a clean exit so the bounded history window is
// backfilled once rather than at every restart. See clearClaudeWatcherState for
// the full reasoning — both watchers hold the identical invariant.
func clearCodexWatcherState() {
	_ = os.Remove(codexWatcherStatePath())
}

func isCodexWatcherRunning() (codexWatcherState, bool) {
	st, err := loadCodexWatcherState()
	if err != nil || st.PID <= 0 {
		return codexWatcherState{}, false
	}
	if processExists(st.PID) {
		return st, true
	}
	clearCodexWatcherState()
	return codexWatcherState{}, false
}

// runCodexWatcher is the main loop for the `promptster codex-watch` subcommand.
// It tails codex rollout JSONL files whose recorded cwd is inside the workspace,
// normalizes each new line, and ingests the resulting events.
func RunCodexWatcher() error {
	session, err := loadSession()
	if err != nil {
		return fmt.Errorf("no active session: %w", err)
	}
	if session.TaskRoot == "" {
		return fmt.Errorf("session has no task root")
	}
	if st, ok := isCodexWatcherRunning(); ok && st.PID != os.Getpid() {
		return fmt.Errorf("codex watcher already running (pid %d)", st.PID)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := saveCodexWatcherState(codexWatcherState{
		PID: os.Getpid(), StartedAt: now, WatchDir: session.TaskRoot,
		LogPath: codexWatcherLogPath(), LastHeartbeat: now,
	}); err != nil {
		return err
	}
	defer clearCodexWatcherState()

	if os.Getenv("PROMPTSTER_API_URL") == "" && session.ApiURL != "" {
		_ = os.Setenv("PROMPTSTER_API_URL", session.ApiURL)
	}

	// Resolve the workspace path through symlinks once (macOS /tmp -> /private/tmp)
	// so cwd comparison against rollout session_meta is reliable.
	workspace := resolvePath(session.TaskRoot)

	// SIGTERM as well as SIGINT — see the matching note in RunClaudeWatcher.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	client := &http.Client{Timeout: 5 * time.Second}
	// One processor per rollout file, kept in memory for the daemon's life so
	// tool-call/output correlation survives across polls.
	processors := map[string]*normalize.CodexRolloutProcessor{}
	eventsCaptured := 0
	// Codex rate-limit *window* emission: a throttled scan of the rollout logs
	// for the latest token_count.rate_limits, mapped to the provider-agnostic
	// windowUsage event. Independent of the per-line capture above (window state
	// is account-global, not workspace-scoped). See window_usage.go.
	var windowEmitter codexWindowEmitter

	// Org capture policy (opt-in assistant prose), fail-closed. Refreshed in the
	// background (immediate + every RefreshInterval) so the poll loop never
	// blocks on the 15s-timeout policy fetch; each iteration reads the
	// lock-guarded cached bool and threads it into every projected event via
	// tailCodexRollout -> AppendEventToLocalBuffer.
	//
	// Built BEFORE the drain because the drain reads its batch-ingest capability.
	policyResolver := policy.NewResolver(session.SessionToken)

	// Delivery runs off the poll loop — see the claude watcher for the full
	// rationale. Both watchers share ONE device-wide queue and run as goroutines
	// in the same supervisor process, so StartDrain is a process-wide singleton:
	// whichever watcher gets there first starts the only drain, and it delivers
	// both watchers' events.
	outbox.StartDrain(client, session.SessionToken, policyResolver.BatchIngest)
	policyCtx, cancelPolicy := context.WithCancel(context.Background())
	defer cancelPolicy()
	policyResolver.StartBackground(policyCtx)

	if verboseWatch() {
		fmt.Fprintf(os.Stderr, "codex-watcher: started, polling %s every %s (workspace=%s)\n",
			codexSessionsDir(), codexWatchInterval, workspace)
	}

	for {
		captureProse := policyResolver.CaptureAssistantProse()
		// Backfill the same bounded history the product visualizes. The rollout's
		// session_meta timestamp, not file mtime, is the authoritative age gate.
		// Recomputed every poll so the window actually ROLLS; frozen at boot it
		// is an absolute date a long-lived daemon drifts away from.
		historyCutoff := transcriptHistoryCutoff(time.Now().UTC())
		queued := pollCodexRollouts(session, workspace, historyCutoff, processors, captureProse)
		eventsCaptured += queued
		windowEmitter.maybe(session, time.Now(), captureProse)

		_ = saveCodexWatcherState(codexWatcherState{
			PID: os.Getpid(), StartedAt: now, WatchDir: session.TaskRoot,
			LogPath:       codexWatcherLogPath(),
			LastHeartbeat: time.Now().UTC().Format(time.RFC3339Nano), EventsCaptured: eventsCaptured,
		})

		select {
		case <-signals:
			fmt.Fprintf(os.Stderr, "codex-watcher: shutting down (captured %d events)\n", eventsCaptured)
			return nil
		case <-time.After(codexWatchInterval):
		}
	}
}

// candidateCodexRollouts lists rollout files under dir modified at/after the
// cutoff, MOST RECENTLY MODIFIED FIRST.
//
// The ORDER is load-bearing. filepath.Walk yields lexical order, and a rollout's
// name begins with its start timestamp — so walking it directly is chronological
// ASCENDING: the oldest history in the window is read before anything recent.
// The poll loop spends a bounded byte budget (codexWatchMaxBytesPerPoll) in that
// order, so during a full-window replay the budget goes entirely to the far end
// of the window while the engineer's current work sits unread behind it. Every
// freshness-gated surface downstream (sessions.last_activity_at, the manager
// activity band) then reads them as inactive for as long as the replay runs.
//
// Descending by recency fills the window from the present backwards: the span
// every default dashboard reads is whole within a poll or two, and interrupting
// the replay at any point leaves a contiguous RECENT window rather than a
// contiguous ancient one.
//
// ModTime is the ordering key, deliberately NOT the session_meta timestamp that
// gates AGE during classification. Ordering needs only a cheap monotonic proxy
// for "how recently was this appended to", it is already what the candidate
// filter below trusts, and reading session_meta here would mean opening every
// rollout in the window on every poll just to decide what to open.
//
// Order WITHIN a file is untouched and stays ascending — CodexRolloutProcessor
// correlates tool calls to their outputs by arrival order.
//
// Mirrors candidateClaudeTranscripts; both watchers hold the identical invariant.
func candidateCodexRollouts(dir string, historyCutoff time.Time) []string {
	type candidate struct {
		path    string
		modTime time.Time
	}
	var found []candidate
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if !strings.HasPrefix(base, "rollout-") || !strings.HasSuffix(base, ".jsonl") {
			return nil
		}
		// Cheap candidate filter: skip files last modified before the history
		// window WITHOUT caching a decision — a file touched later re-enters
		// classification. Caching "no" here is the old bug that dropped
		// long/resumed/restart-spanning rollouts forever (mirrors
		// candidateClaudeTranscripts).
		if info.ModTime().Before(historyCutoff) {
			return nil
		}
		found = append(found, candidate{path: path, modTime: info.ModTime()})
		return nil
	})
	// Path descending as the tie-break: mtime granularity can collide across
	// files written in the same instant, and an unstable order would make the
	// per-poll budget fall on a different rollout each poll.
	sort.Slice(found, func(i, j int) bool {
		if found[i].modTime.Equal(found[j].modTime) {
			return found[i].path > found[j].path
		}
		return found[i].modTime.After(found[j].modTime)
	})
	out := make([]string, 0, len(found))
	for _, c := range found {
		out = append(out, c.path)
	}
	return out
}

// pollCodexRollouts scans for rollout files, tails matched ones from their last
// byte offset, and ingests normalized events. Returns the number sent.
func pollCodexRollouts(
	session Session,
	workspace string,
	historyCutoff time.Time,
	processors map[string]*normalize.CodexRolloutProcessor,
	captureProse bool,
) int {
	dir := codexSessionsDir()
	progress := loadCodexWatchProgress()
	sent := 0
	// Shared per-poll read budget; zeroed while the queue is under pressure so
	// the backfill defers instead of pushing the outbox to its dropping cap.
	// The rollout bytes stay on disk with the offset unmoved, so a deferred read
	// is never a lost read. See claudeWatchMaxBytesPerPoll.
	budget := codexWatchMaxBytesPerPoll
	if outbox.UnderPressure() {
		budget = 0
	}
	deferredWork := false
	// Byte-throttled mid-poll checkpoints — see transcriptProgressCheckpointBytes
	// and the Claude half for why accruing and flushing are separate steps.
	//
	// The separation is sharper here: session_meta is the ONLY rollout record
	// carrying cwd, so a flush landing between the cursor advancing past it and
	// the decision being written is unrecoverable. The next poll resumes AFTER
	// the header, finds no other cwd anywhere in the file, spends
	// codexClassifyMaxSkips and caches "no" for good. The Claude half survives
	// the same crash only because its later records repeat cwd.
	unsaved := int64(0)
	accrue := func(consumed int64) { unsaved += consumed }
	checkpoint := func() {
		if unsaved >= transcriptProgressCheckpointBytes {
			unsaved = 0
			saveCodexWatchProgress(progress)
		}
	}
	// One oversized-record probe per poll — see the Claude half.
	oversizeProbe := true
	// Same root set the Claude watcher matches against: the workspace, every
	// directory registered by a later `start`, and their git worktrees. Codex
	// used to compare against the single workspace, so a registered second tree
	// would have stayed invisible here even after the Claude side saw it.
	roots := workspaceMatchRoots(workspace)
	if fp, dropped, changed := syncMatchCacheToRoots(progress.Match, progress.RootsFP, roots); changed {
		if dropped > 0 {
			fmt.Fprintf(os.Stderr, "codex-watcher: capture roots changed — re-checking %d cached rollout(s)\n", dropped)
		}
		progress.RootsFP = fp
		saveCodexWatchProgress(progress)
	}

	candidates := candidateCodexRollouts(dir, historyCutoff)

	processRollout := func(path string) {
		switch progress.Match[path] {
		case "no":
			return
		case "yes":
			// proceed to tail
		default:
			if budget <= 0 {
				deferredWork = true
				return
			}
			remaining := budget
			classifyBudget := budget
			if oversizeProbe && classifyBudget < transcriptMaxRecordBytes {
				classifyBudget = transcriptMaxRecordBytes
			}
			match, res, scanned := classifyCodexRolloutBounded(
				path, roots, historyCutoff,
				progress.ClassifyOffsets[path], progress.ClassifyScanned[path], classifyBudget,
				oversizeProbe, progress.ClassifyDiscarding[path],
			)
			progress.ClassifyOffsets[path] += res.consumed
			progress.ClassifyScanned[path] = scanned
			if res.discardingOversize {
				progress.ClassifyDiscarding[path] = true
			} else {
				delete(progress.ClassifyDiscarding, path)
			}
			budget -= res.consumed
			if res.probedOversize || res.consumed > remaining {
				oversizeProbe = false
			}
			accrue(res.consumed)
			switch match {
			case codexMatchYes:
				progress.Match[path] = "yes"
			case codexMatchYesPreexisting:
				// Go-forward: capture ongoing activity but not out-of-window
				// history. Seed the offset to current EOF so tailing starts at new
				// content. Only when unseen — a real prior offset (a restart-spanning
				// session already being tailed) must be preserved. If the stat fails
				// transiently, DON'T cache "yes" yet: leave the match undecided and
				// retry next poll, so a later success seeds EOF instead of tailing the
				// whole old file from offset 0. The retry has to start classification
				// OVER — see clearCodexClassifyState.
				if _, ok := progress.Offsets[path]; !ok {
					info, err := os.Stat(path)
					if err != nil {
						clearCodexClassifyState(progress, path)
						checkpoint()
						return
					}
					progress.Offsets[path] = info.Size()
				}
				progress.Match[path] = "yes"
			case codexMatchNo:
				progress.Match[path] = "no"
			default: // undecided — no readable session_meta yet; retry next poll
				checkpoint()
				return
			}
			clearCodexClassifyState(progress, path)
			checkpoint()
			if match == codexMatchNo {
				return
			}
		}

		if budget <= 0 {
			// Budget spent: leave this rollout's offset untouched so its unread
			// bytes re-surface next poll. Deferred, never dropped. Checked before
			// the processor is built, because a cold processor replays the whole
			// consumed prefix and shells out to git for the repo identity.
			deferredWork = true
			return
		}

		proc := processors[path]
		if proc == nil {
			proc = normalize.NewCodexRolloutProcessor(codexSessionIDFromPath(path))
			// Progress offsets survive daemon restarts, while the processor's
			// call/output correlation maps do not. Replay the consumed prefix only
			// to rebuild that transient state; deterministic events are discarded
			// and tailing still begins at the persisted offset below.
			if offset := progress.Offsets[path]; offset > 0 {
				replayCodexRolloutPrefix(path, offset, proc)
			}
			// Resolve the canonical repo identity ONCE per session (this processor is
			// created once per rollout file) from the session_meta cwd, and thread it
			// in as session state so each prompt event carries repoRoot + repoHost +
			// repoTracked. All three parts come from ONE call so the host and the
			// tracked bit can never be stamped from a different resolution pass than
			// the slug — they describe one observation of one directory, and a second
			// pass could see it after a `git init`.
			proc.RepoRoot, proc.RepoHost, proc.RepoTracked = sessionRepoIdentity(codexRolloutCwd(path))
			processors[path] = proc
		}
		n, res := tailCodexRollout(path, progress, proc, session, captureProse, budget, oversizeProbe)
		sent += n
		budget -= res.consumed
		if res.probedOversize {
			oversizeProbe = false
		}
		if res.truncated {
			deferredWork = true
		}
		// Checkpoint mid-poll: an unsaved offset replays from byte zero after a
		// SIGKILL, which is what stops a restart loop ever finishing a long
		// backfill.
		accrue(res.consumed)
		checkpoint()
	}

	for _, path := range candidates {
		processRollout(path)
	}

	if deferredWork && verboseWatch() {
		fmt.Fprintf(os.Stderr, "codex-watcher: per-poll read budget spent (%d bytes) — remaining rollout history deferred to the next poll\n",
			codexWatchMaxBytesPerPoll)
	}

	saveCodexWatchProgress(progress)
	return sent
}

// replayCodexRolloutPrefix reconstructs a fresh processor's transient state
// after a watcher restart. It applies the same pre-parse redaction as live
// tailing and intentionally discards every event, so already-consumed records
// are never queued twice.
func replayCodexRolloutPrefix(path string, offset int64, proc *normalize.CodexRolloutProcessor) {
	// #nosec G304 -- path is a Codex rollout discovered under the sessions dir and is opened read-only.
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	reader := bufio.NewReader(io.LimitReader(f, offset))
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			trimmed := strings.TrimSpace(string(line))
			if trimmed != "" {
				_ = proc.Process(redact.RedactBytes([]byte(trimmed)))
			}
		}
		if err != nil {
			return
		}
	}
}

type codexMatchResult int

const (
	codexMatchUndecided      codexMatchResult = iota
	codexMatchYes                             // matched; inside the history window — tail from the start
	codexMatchYesPreexisting                  // matched; older than the history window — capture go-forward from EOF
	codexMatchNo
)

// classifyCodexRollout decides whether a rollout belongs to this capture
// session by reading its first classifiable record — the session_meta header,
// the ONLY rollout record carrying cwd and the session start timestamp. Records
// too large for any pass to parse are skipped to reach it (nextClassifyRecord);
// without that, one ahead of the header stalled the file forever.
//
// cwd is authoritative. The timestamp admits sessions in the bounded history
// window from byte zero; older matched sessions are returned as
// codexMatchYesPreexisting and captured go-forward from current EOF.
//
// Unlike the Claude watcher's multi-line scan, cwd + timestamp both live on the
// header only, so a file caught mid-creation whose first record is not yet a
// readable session_meta is returned codexMatchUndecided (retry next poll) rather
// than cached as a mismatch — caching "no" would drop it forever.
//
// This is the CONVENIENCE classifier: it re-reads from byte zero, so "retry next
// poll" is the only deferral it has. The poll loop runs
// classifyCodexRolloutBounded instead, which owns a durable cursor and therefore
// resolves the same record by SKIPPING it under a bound. Both refuse to cache a
// "no" off one unreadable record; only the bounded one can also make progress
// past it.
func classifyCodexRollout(path string, roots []string, historyCutoff time.Time) codexMatchResult {
	// #nosec G304 -- path is a Codex rollout file discovered under the Codex sessions dir by the watcher, not user input; opened read-only.
	f, err := os.Open(path)
	if err != nil {
		return codexMatchUndecided
	}
	defer f.Close()
	line, ok := firstClassifiableCodexRecord(newClassifyReader(f))
	if !ok {
		return codexMatchUndecided
	}
	if line == nil {
		// Nothing but unsupported records where the header belongs. A rollout
		// header is a small record, so this file is not one — and caching the
		// answer is what stops the every-poll re-read this bound exists to
		// prevent. Fail closed: no cwd was ever read, so nothing is captured.
		return codexMatchNo
	}
	var rec struct {
		Timestamp string `json:"timestamp"`
		Type      string `json:"type"`
		Payload   struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(line, &rec); err != nil {
		return codexMatchUndecided
	}
	if rec.Type != "session_meta" || rec.Payload.Cwd == "" {
		return codexMatchUndecided
	}
	if !pathWithinAny(resolvePath(rec.Payload.Cwd), roots) {
		return codexMatchNo
	}
	// Replay in-window history from the start. Anything older — INCLUDING a
	// rollout whose session_meta timestamp will not parse — remains eligible for
	// go-forward capture without uploading its historical prefix. The gate fails
	// CLOSED for the same reason the Claude one does: mtime is the only other
	// bound, and a months-old session resumed today has today's mtime.
	t, err := time.Parse(time.RFC3339, rec.Timestamp)
	if err != nil || t.Before(historyCutoff) {
		return codexMatchYesPreexisting
	}
	return codexMatchYes
}

// classifyCodexRolloutBounded is the durable poll-path classifier. It keeps a
// cursor distinct from the replay offset so an unsupported leading record can
// be skipped over multiple bounded polls without forfeiting history.
//
// A record that is READABLE but is not the session_meta header is a SKIP, not a
// verdict, and that is the one place this deliberately does more than its
// unbounded sibling above rather than less. The sibling has no cursor, so its
// only options on such a record are "undecided" (re-read the file from byte
// zero every 3s, forever) or "no" (drop the session, forever); it takes the
// recoverable one. The cursor here supplies a third: step past the record and
// keep looking, with codexClassifyMaxSkips bounding the search so an
// unclassifiable file still reaches a decision. Both halves therefore obey the
// same rule — a single odd record never costs a whole session — and neither can
// read without end.
func classifyCodexRolloutBounded(
	path string,
	roots []string,
	historyCutoff time.Time,
	offset int64,
	skipped int,
	budget int64,
	oversizeProbe bool,
	discarding bool,
) (codexMatchResult, transcriptReadOutcome, int) {
	// #nosec G304 -- candidate path discovered under the Codex sessions dir.
	f, err := os.Open(path)
	if err != nil {
		return codexMatchUndecided, transcriptReadOutcome{}, skipped
	}
	defer f.Close()
	if _, err := f.Seek(offset, 0); err != nil {
		return codexMatchUndecided, transcriptReadOutcome{}, skipped
	}

	result := codexMatchUndecided
	out := readTranscriptRecords(f, budget, oversizeProbe, discarding, func(line []byte, _ int64) bool {
		if result != codexMatchUndecided {
			return false
		}
		skipped++
		if skipped > codexClassifyMaxSkips+1 {
			result = codexMatchNo
			return false
		}
		var rec struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Payload   struct {
				Cwd string `json:"cwd"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &rec) != nil {
			return true
		}
		if rec.Type != "session_meta" || rec.Payload.Cwd == "" {
			return true
		}
		if !pathWithinAny(resolvePath(rec.Payload.Cwd), roots) {
			result = codexMatchNo
			return false
		}
		t, err := time.Parse(time.RFC3339, rec.Timestamp)
		if err != nil || t.Before(historyCutoff) {
			result = codexMatchYesPreexisting
			return false
		}
		result = codexMatchYes
		return false
	})
	return result, out, skipped
}

// codexClassifyMaxSkips bounds how many records a classification pass steps
// over before giving up on finding the session_meta header — unsupported, blank
// or merely not the header. It is what keeps an unclassifiable file from being
// scanned without end, the same job claudeClassifyMaxScanLines does on the
// Claude side. The bounded classifier spends it across polls (its cursor is
// durable); the convenience one spends it within a single pass.
const codexClassifyMaxSkips = 50

// firstClassifiableCodexRecord returns the first record a rollout's header
// could be. ok false means the file ended mid-record — still being written, so
// retry next poll. A nil record with ok true means the skip bound was reached
// without one, which is a decision, not a deferral.
func firstClassifiableCodexRecord(r *bufio.Reader) ([]byte, bool) {
	for skipped := 0; skipped <= codexClassifyMaxSkips; skipped++ {
		line, ok := nextClassifyRecord(r)
		if !ok {
			return nil, false
		}
		if len(line) > 0 {
			return line, true
		}
	}
	return nil, true
}

// codexRolloutCwd returns the absolute cwd recorded in a rollout file's
// session_meta header (the only rollout record carrying cwd), or "" when no
// readable session_meta appears within the classifier's skip bound. Read-only,
// header only — the body is never retained; the caller reduces the cwd to a
// privacy-safe repo identity.
//
// It walks to the header exactly the way classifyCodexRolloutBounded does —
// past unsupported records, past readable non-header ones, AND past a
// session_meta carrying no cwd, under the same codexClassifyMaxSkips bound.
// Every one of those is a skip there too, and any of them treated as a stop
// here would admit a rollout the classifier only reached the header PAST with
// no repo identity at all.
func codexRolloutCwd(path string) string {
	// #nosec G304 -- path is a Codex rollout file discovered under the Codex sessions dir by the watcher, not user input; opened read-only and only the cwd field is read.
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	reader := newClassifyReader(f)
	for skipped := 0; skipped <= codexClassifyMaxSkips; skipped++ {
		line, ok := nextClassifyRecord(reader)
		if !ok {
			return ""
		}
		if len(line) == 0 {
			continue
		}
		var rec struct {
			Type    string `json:"type"`
			Payload struct {
				Cwd string `json:"cwd"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &rec) != nil || rec.Type != "session_meta" || rec.Payload.Cwd == "" {
			continue
		}
		return rec.Payload.Cwd
	}
	return ""
}

// tailCodexRollout reads new complete lines from path (starting at the stored
// offset), processes them, queues resulting events, and advances the offset.
// A trailing partial line (no newline yet) is left for the next poll. Returns
// the number of events queued plus the shared read outcome (see
// readTranscriptRecords — one implementation for both rails).
//
// budget caps the bytes THIS call may read so one enormous rollout cannot spend
// a whole poll before the caller's loop gets to check. Stopping early is safe
// for the same reason the offset advance is: unread bytes stay on disk and the
// offset stops short of them. oversizeProbe carries the poll's single allowance
// to read past that budget, and only to establish that one record exceeds
// transcriptMaxRecordBytes.
//
// As in the claude watcher, advancing the offset over a QUEUED record is only
// safe because delivery is now durable: this loop used to POST inline and
// advance regardless, so any 429/5xx/timeout destroyed the event permanently.
// A record the outbox REFUSED (outbox.ErrQueueFull) is the exception and is
// rewound rather than committed — see the block inside.
func tailCodexRollout(
	path string,
	progress codexWatchProgress,
	proc *normalize.CodexRolloutProcessor,
	session Session,
	captureProse bool,
	budget int64,
	oversizeProbe bool,
) (int, transcriptReadOutcome) {
	// #nosec G304 -- path is a Codex rollout file discovered under the Codex sessions dir by the watcher, not user input; opened read-only.
	f, err := os.Open(path)
	if err != nil {
		return 0, transcriptReadOutcome{}
	}
	defer f.Close()

	offset := progress.Offsets[path]
	if _, err := f.Seek(offset, 0); err != nil {
		return 0, transcriptReadOutcome{}
	}

	queued := 0
	emit := func(ev event.Event) (int, error) { return emitCodexEvent(ev, session, captureProse) }
	// queueFullAt is the read-relative start of the FIRST record whose events the
	// outbox refused, or -1 while none has. See the rewind below.
	queueFullAt := int64(-1)
	wasDiscarding := progress.Discarding[path]
	res := readTranscriptRecords(f, budget, oversizeProbe, wasDiscarding, func(record []byte, recordStart int64) bool {
		// Scrub secrets before parsing/ingest — same redaction the hook path
		// applies. Rollout lines carry prompt text, command output, and file
		// patches that may contain keys/tokens the candidate pasted or printed.
		redacted := redact.RedactBytes(record)
		for _, ev := range proc.Process(redacted) {
			n, err := emit(ev)
			queued += n
			if errors.Is(err, outbox.ErrQueueFull) {
				queueFullAt = recordStart
				return false
			}
		}
		return true
	})

	// A full outbox means this byte range was NOT consumed — same rule and same
	// reason as the Claude rail (see tailClaudeTranscript). The queue DROPPED the
	// event, so committing the offset over the record that produced it would file
	// it as read while nothing holds it any more, turning a recoverable stall
	// into permanent loss. The rewind stops at the FIRST failing record because
	// an offset is a single number and cannot express a hole; re-reading the rest
	// next poll is free, since the normalizer derives stable per-record event ids
	// and the backend collapses a re-queued event onto the same row.
	//
	// Applied BEFORE the stale-prompt flush below, deliberately: that flush
	// RELEASES in-memory state, and an event released into a full queue is gone
	// for good — the rollout bytes that would rebuild it are exactly the ones
	// this rewind has just declined to consume.
	if queueFullAt >= 0 {
		if res.consumed > queueFullAt {
			res.consumed = queueFullAt
		}
		res.truncated = true
	}

	if res.discarded > 0 {
		fmt.Fprintf(os.Stderr, "codex-watcher: discarded %d bytes of an unsupported record (over %d bytes) in %s\n",
			res.discarded, transcriptMaxRecordBytes, filepath.Base(path))
	}

	// Release a recovered user turn the next line will never come for. Runs on
	// EVERY poll, including polls that consumed nothing — a rollout whose final
	// line is the human's turn produces no further lines to flush it, so an
	// end-of-read hook that only fired after new bytes would never reach it.
	// Skipped when the budget cut the read short: the lines that would complete
	// that turn are sitting unread in the file, not absent.
	if !res.truncated {
		for _, ev := range proc.FlushStaleUserPrompt() {
			// Error ignored on purpose: this event came out of in-memory state
			// the flush has already released, so there is no byte range to
			// rewind to and nothing better to do than outbox's own warning.
			n, _ := emit(ev)
			queued += n
		}
	}

	if res.consumed > 0 {
		progress.Offsets[path] = offset + res.consumed
		if queued > 0 && verboseWatch() {
			fmt.Fprintf(os.Stderr, "codex-watcher: queued %d event(s) from %s\n", queued, filepath.Base(path))
		}
	}
	if res.discardingOversize {
		progress.Discarding[path] = true
	} else {
		delete(progress.Discarding, path)
	}
	return queued, res
}

// emitCodexEvent stamps, dedupes, ledgers and queues one normalized event, and
// reports how many events it queued (0 or 1) plus the QUEUE's error, which the
// tail reads to decide whether the record it came from may be marked consumed.
// Shared by the line-by-line tail and the stale-prompt flush so a recovered
// prompt goes through the IDENTICAL path as every other event — same device
// stamping, same path relativization, same signing.
func emitCodexEvent(ev event.Event, session Session, captureProse bool) (int, error) {
	// SessionID comes from the rollout; DeviceID comes from the environment.
	// Stamped here rather than in the normalizer, which has no business knowing
	// what machine it runs on — keeping the two sourced separately is what stops
	// them collapsing into one value.
	ev.DeviceID = session.DeviceID
	normalize.RelativizeEventPaths(&ev, session.TaskRoot)
	// Record AI bash execution windows for later commit-attribution recovery —
	// same as the Claude watcher. No-op unless this is an AI-attributed
	// `command` event.
	// Codex stamps every rollout record with its own timestamp, so the bounded
	// history replay is separable from the live tail per event — same terms as
	// the Claude funnel.
	replay := transcriptEventIsHistorical(&ev)
	recordAiBashWindow(&ev, session.TaskRoot, replay)
	// Idempotency: skip a file_diff whose resulting content the git watcher (or
	// another channel) has already emitted, so an apply_patch edit isn't
	// double-counted when the working-tree poll sees it later.
	if !dedupeFileDiff(session.TaskRoot, &ev, replay) {
		return 0, nil
	}
	// Ledger first — it projects, scrubs, and signs ev in place, so the queued
	// copy is the exact bytes to ship. See queueClaudeWatchEvent.
	if err := sign.AppendEventToLocalBuffer(&ev, captureProse); err != nil {
		fmt.Fprintf(os.Stderr, "codex-watcher: buffer error: %v\n", err)
	}
	// Durable source — the rollout file is on disk and, on ErrQueueFull, the
	// caller declines to move the offset past this record. Same terms as
	// queueClaudeWatchEvent; see the note there.
	if err := outbox.AppendFromDurableSource(ev); err != nil {
		fmt.Fprintf(os.Stderr, "codex-watcher: queue error (%s): %v\n", ev.Kind, err)
		return 0, err
	}
	return 1, nil
}

// resolvePath resolves symlinks (falling back to a cleaned path) so workspace
// comparisons survive macOS's /tmp -> /private/tmp aliasing.
func resolvePath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// pathWithinAny reports whether child sits at or under ANY of roots. An empty
// root list matches nothing — a watcher with no resolvable roots captures
// nothing rather than everything.
func pathWithinAny(child string, roots []string) bool {
	for _, r := range roots {
		if r != "" && pathWithin(child, r) {
			return true
		}
	}
	return false
}

// PathWithin exports pathWithin for callers outside this package (the status
// row folds registered roots that the daemon root already covers).
func PathWithin(child, parent string) bool { return pathWithin(child, parent) }

// pathWithin reports whether child is the same as or nested under parent.
func pathWithin(child, parent string) bool {
	child = filepath.Clean(child)
	parent = filepath.Clean(parent)
	if child == parent {
		return true
	}
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}
