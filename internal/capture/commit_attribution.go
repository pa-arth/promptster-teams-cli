package capture

import (
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/outbox"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// Post-commit AI attribution.
//
// When the out-of-band git watcher detects a new commit C, this engine computes
// per-file line-range attribution by reconciling AI evidence against the REAL
// committed diff, and emits ONE content-free `commit_attribution` event over the
// durable outbox. It is deliberately OFF any latency-sensitive path (the watcher
// polls on a ~60s timer), so its single `git show` per commit is affordable.
//
// The committed diff is the source of truth: it already contains any formatter
// reflow (a silent `oxlint --fix` PostToolUse hook, `gofmt`, …), so reconciling
// against it is formatter-robust BY CONSTRUCTION — the emitted ranges are the
// spans that actually landed in the commit, never the pre-formatter transcript
// ranges, which would not line up 1:1 anyway.
//
// HARD PRIVACY RULE: the event carries a PUBLIC commit hash, the privacy-safe
// workspaceKey (git slug or opaque hash — never a path), and content-free
// {start,end,attribution} line ranges. It NEVER carries diff bytes, file
// contents, the commit message, author, or any old/new string. Only `@@` hunk
// headers and `+++ b/<path>` file headers are parsed out of the diff; the +/-
// body bytes are read into memory and discarded, never emitted.
//
// ATTRIBUTION VALUES: exactly two — `likely_ai` and `unknown`. There is no
// editor-extension known-human signal in this codebase, and "not-AI" is a
// superset (pre-existing code, merges, third-party, missed-AI, human-typed), so
// collapsing it to `human` would be a lie about provenance that biases AI% up.
// Residue is ALWAYS `unknown`.

// attrLineRange is one content-free changed-line span on the NEW (committed)
// side of a file, tagged with an attribution enum. Ints + one enum only.
type attrLineRange struct {
	Start       int    `json:"start"`
	End         int    `json:"end"`
	Attribution string `json:"attribution"`
}

// attrFile is the attribution for one changed file in a commit.
type attrFile struct {
	Path       string          `json:"path"`
	LineRanges []attrLineRange `json:"lineRanges"`
}

// commitAttributionData is the CLOSED payload of a commit_attribution event.
type commitAttributionData struct {
	CommitSha    string     `json:"commitSha"`
	WorkspaceKey string     `json:"workspaceKey"`
	Files        []attrFile `json:"files"`
}

const (
	attributionLikelyAI = "likely_ai"
	attributionUnknown  = "unknown"
)

// diffHunkRe captures the NEW-file-side start (group 1) and optional line count
// (group 2) of a unified-diff hunk header: `@@ -a,b +c,d @@`. A missing `,d`
// means a single line; d==0 is a pure deletion (no new-side lines).
var diffHunkRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// gitCommitDiffRanges runs ONE `git show` for the commit and returns the
// new-file-side changed line ranges per repo-relative path (attribution unset —
// reconcile fills it). One spawn per commit, never per file/line. `--root`
// diffs the parentless first commit against the empty tree; `--format=`
// suppresses the commit header (message/author never enter the buffer);
// `--unified=0` makes the `@@` spans tight to the changed lines. Best-effort:
// an error (bad sha, not a repo) yields an empty map, which suppresses emission.
func gitCommitDiffRanges(root, sha string) map[string][]attrLineRange {
	// core.quotePath=false keeps non-ASCII paths verbatim (UTF-8) instead of
	// C-quoted (octal-escaped), so they match the ledger's relPath keys. Paths
	// with literal tabs/quotes/backslashes are still double-quoted by git and
	// remain a rare documented miss.
	// #nosec G204 -- constant argv; root is a discovered workspace dir and sha comes from git rev-list output (the watcher), not user input. Read-only.
	out, err := exec.Command("git", "-C", root,
		"-c", "core.quotePath=false",
		"show", "--root", "--no-color", "--unified=0", "--format=", sha).Output()
	if err != nil {
		return map[string][]attrLineRange{}
	}
	return parseUnifiedDiffNewRanges(string(out))
}

// parseUnifiedDiffNewRanges extracts, per changed file, the NEW-side changed
// line ranges from a unified diff. It reads ONLY the `diff --git` anchor, the
// `+++ b/<path>` file header, and the `@@` hunk headers — every +/- body line is
// ignored. Path is taken from the first `+++ ` line after each `diff --git`
// anchor and BEFORE that file's first `@@`, so a body line that happens to start
// with `+++ ` (an added line whose content begins `++ `) can never be mistaken
// for a header. A `/dev/null` new side (a deletion) contributes no ranges.
func parseUnifiedDiffNewRanges(diff string) map[string][]attrLineRange {
	out := map[string][]attrLineRange{}
	current := ""
	inBody := false // true once this file's first @@ has been seen
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			current = ""
			inBody = false
		case !inBody && strings.HasPrefix(line, "+++ "):
			current = parseDiffNewPath(line[len("+++ "):])
		case strings.HasPrefix(line, "@@ "):
			inBody = true
			if current == "" {
				continue
			}
			m := diffHunkRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			start, _ := strconv.Atoi(m[1])
			count := 1
			if m[2] != "" {
				count, _ = strconv.Atoi(m[2])
			}
			if count <= 0 {
				continue // pure deletion — no new-side lines to attribute
			}
			out[current] = append(out[current], attrLineRange{Start: start, End: start + count - 1})
		}
	}
	return out
}

// parseDiffNewPath reduces a `+++ ` header's target to a repo-relative POSIX
// path. Git prefixes the new side with `b/`; a deletion targets `/dev/null`,
// which yields "" (no new-side path). Git already emits forward slashes.
func parseDiffNewPath(s string) string {
	s = strings.TrimSpace(s)
	if s == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(s, "b/")
}

// reconcileCommitAttribution tags each changed file's committed ranges against
// the AI-touched-paths ledger and returns the per-file attributions (sorted by
// path for a stable, reviewable payload) plus the representative AI session.
//
// PATH-LEVEL v1 (deliberate): a changed file is either AI-touched (→ all its
// committed ranges are likely_ai) or not (→ unknown). Per-line intersection of
// transcript ranges vs committed ranges is a later refinement — the transcript
// ranges are pre-formatter and won't line up 1:1 with the committed lines
// anyway, so path-level + committed-bytes is the defensible first cut.
func reconcileCommitAttribution(fileRanges map[string][]attrLineRange, aiPaths map[string]string) (files []attrFile, primarySession string) {
	paths := make([]string, 0, len(fileRanges))
	for p := range fileRanges {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	sessionFiles := map[string]int{} // sessionId -> how many files in this commit it touched
	files = make([]attrFile, 0, len(paths))
	for _, path := range paths {
		attribution := attributionUnknown
		// PATH-SPACE NOTES (what's true after workspace scoping):
		//   1. aiPaths is now scoped by workspace root key (the caller passes
		//      gitWatchRootKey(root) into readAiTouchedPaths), so a same-named
		//      path AI-touched in ANOTHER repo can no longer bleed in.
		//   2. Remaining v1 limitation (temporal, SAME workspace): a file
		//      AI-touched earlier and then edited by a HUMAN within the 7-day
		//      ledger TTL still attributes `likely_ai` at path granularity. The
		//      Phase-2 commit-joined ledger will resolve this per-line/per-commit.
		//   3. Sibling-worktree note: aiPaths keys are relativized against
		//      session.TaskRoot, while `path` is relative to the git root polled.
		//      For a SIBLING worktree (root != TaskRoot) the keys differ, so AI
		//      evidence there reads as `unknown` — a conservative under-attribution.
		if sid, ok := aiPaths[path]; ok {
			attribution = attributionLikelyAI
			sessionFiles[sid]++
		}
		src := fileRanges[path]
		tagged := make([]attrLineRange, 0, len(src))
		for _, r := range src {
			r.Attribution = attribution
			tagged = append(tagged, r)
		}
		files = append(files, attrFile{Path: path, LineRanges: tagged})
	}
	return files, mostFrequentSession(sessionFiles)
}

// mostFrequentSession returns the sessionId that touched the most files in the
// commit, tie-broken by the lexicographically smallest id so the choice is
// deterministic. "" when no AI session touched the commit.
func mostFrequentSession(counts map[string]int) string {
	best, bestN := "", 0
	for sid, n := range counts {
		if n > bestN || (n == bestN && (best == "" || sid < best)) {
			best, bestN = sid, n
		}
	}
	return best
}

// buildCommitAttributionEvent reconciles commit sha against the AI-paths ledger
// and returns the ready-to-funnel event. ok is false when the commit changed no
// attributable files (an empty diff, or a merge whose combined diff this parser
// intentionally does not attribute) — the caller then emits nothing.
//
// The event's sessionId is the most-active AI session touching the commit, so
// the backend can key attribution to a real AI-tool session; when no AI session
// touched it (an all-unknown commit) it falls back to the device id, matching
// how the other DEVICE-scoped watcher events (config_census, presence) pick one.
//
// Data goes through eventDataMap (a JSON round-trip) so nested arrays-of-structs
// land as []interface{} of map[string]interface{} — the only shape the redaction
// projector's element allowlist can walk. Assigning the struct straight to Data
// would silently ship {} (see eventDataMap's header).
func buildCommitAttributionEvent(session Session, root, sha string) (event.Event, bool) {
	fileRanges := gitCommitDiffRanges(root, sha)
	if len(fileRanges) == 0 {
		return event.Event{}, false
	}
	// Scope the AI-paths read to THIS workspace's root key so a same-named path
	// AI-touched in a DIFFERENT repo can't bleed in (closes cross-workspace
	// over-attribution). gitWatchRootKey is in this package.
	files, primarySession := reconcileCommitAttribution(fileRanges, readAiTouchedPaths(gitWatchRootKey(root)))

	sessionID := primarySession
	if sessionID == "" {
		sessionID = session.DeviceID
	}
	e := event.NewEvent("commit_attribution", sessionID)
	e.Source = presenceSource
	e.DeviceID = session.DeviceID
	e.Actor = event.SystemActor()
	e.Data = eventDataMap(commitAttributionData{
		CommitSha:    sha,
		WorkspaceKey: workspaceKey(root),
		Files:        files,
	})
	return e, true
}

// emitCommitAttribution runs the event through the SAME buffer/sign/queue funnel
// as every captured event: sign+redact into the local ledger FIRST (mutates ev
// with Sig/PrevSig and strips source), THEN enqueue the exact projected bytes on
// the shared outbox. It reuses the process-wide outbox.StartDrain singleton the
// transcript watchers already started — it never starts its own drain.
func emitCommitAttribution(ev event.Event) {
	if err := sign.AppendEventToLocalBuffer(&ev, false); err != nil {
		state.HookDebugf("commit attribution buffer error: %v", err)
	}
	if err := outbox.Append(ev); err != nil {
		state.HookDebugf("commit attribution queue error: %v", err)
	}
}

// attributeCommit builds and emits attribution for one detected commit, or does
// nothing when there is nothing attributable.
//
// KNOWN COLD-LEDGER HOLE (deliberate boundary of the out-of-band design, not a
// bug): attribution here re-derives from the 7-day AI-paths ledger
// (readAiTouchedPaths) every poll. A rewritten commit that RE-ENTERS via
// rev-list (amend, rebase, squash, cherry-pick) is therefore re-attributed
// correctly only while its files are still inside that 7-day window. Once the
// TTL expires, the same commit attributes `unknown` — a conservative MISS, never
// a misattribution. Cold SQUASH specifically cannot be recovered by patch-id: a
// squashed commit's patch-id is the union diff of its sources and matches no
// single source commit, so there is nothing to match against. Recovering it
// would need an explicit merge/squash signal we do not collect out-of-band.
func attributeCommit(session Session, root, sha string) {
	ev, ok := buildCommitAttributionEvent(session, root, sha)
	if !ok {
		return
	}
	emitCommitAttribution(ev)
}
