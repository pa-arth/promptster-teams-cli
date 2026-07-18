package capture

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/outbox"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// Pre-merge REWORK tracking — the mirror image of durability.
//
// Durability follows AI lines FORWARD on the default branch to measure survival.
// Rework measures the opposite, on the way IN: AI lines that the AI (or a human)
// rewrites on a feature branch BEFORE it merges. A high rework rate means the
// AI's first output needed revision to become mergeable — a quality signal the
// backend joins against GitHub review metadata (§7.1).
//
// It reuses §2's interval machinery wholesale: the same `remapTrackedRanges`
// churn/shift math, the same first-touch-only seeding discipline (re-seeding
// would re-attribute a human's rewrite of an AI line as fresh AI — inflation the
// privacy rules forbid), and the same single-locked read-modify-write ledger. The
// ONLY differences from durability are:
//   - it advances on WORKING HEAD, gated to commits made while the branch is
//     ahead of the default branch (pre-merge) — see pollGitWatchWorkspace;
//   - a churned tracked range emits a rework_verdict immediately (there is no
//     maturity window — a rewrite IS the event);
//   - it reuses the attribution watcher's ALREADY-fetched `git show` diff + files
//     (no extra spawn), so a pre-merge commit stays one `git show`.
//
// PRIVACY: identical to durability — only integer ranges, an age, and a
// `sha:path` lineage handle leave; never bytes, diffs, or content.

const reworkLedgerVersion = 1

// reworkLedger is the on-disk pre-merge rework state: rootKey → path → the AI
// spans currently tracked on the working branch. It needs no cursor of its own —
// it piggybacks on the attribution watcher's working-HEAD detection — and no TTL,
// because it is cleared the moment the branch merges back to the default branch.
type reworkLedger struct {
	V     int                                     `json:"v"`
	Roots map[string]map[string][]durTrackedRange `json:"roots"`
}

func reworkLedgerPath() string {
	return filepath.Join(state.StateDir(), "rework.json")
}

// readReworkLedgerUnlocked reads the ledger WITHOUT the buffer lock — the caller
// must hold it. A missing/version-mismatched file yields an empty ledger.
func readReworkLedgerUnlocked() reworkLedger {
	led := reworkLedger{V: reworkLedgerVersion, Roots: map[string]map[string][]durTrackedRange{}}
	data, err := os.ReadFile(reworkLedgerPath())
	if err != nil {
		return led
	}
	var onDisk reworkLedger
	if json.Unmarshal(data, &onDisk) == nil && onDisk.V == reworkLedgerVersion && onDisk.Roots != nil {
		led = onDisk
	}
	return led
}

// writeReworkLedgerUnlocked writes atomically (tmp + rename) WITHOUT the buffer
// lock — the caller must hold it. Best-effort: I/O failure never blocks.
func writeReworkLedgerUnlocked(led reworkLedger) {
	led.V = reworkLedgerVersion
	data, err := json.Marshal(led)
	if err != nil {
		return
	}
	tmp := reworkLedgerPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, reworkLedgerPath())
}

// loadReworkLedger reads the ledger under the buffer lock. For read-only callers;
// a mutating caller MUST use mutateReworkLedger.
func loadReworkLedger() reworkLedger {
	var led reworkLedger
	_ = sign.WithBufferLock(reworkLedgerPath()+".lock", func() error {
		led = readReworkLedgerUnlocked()
		return nil
	})
	return led
}

// mutateReworkLedger runs load -> fn -> save as ONE locked read-modify-write, so
// concurrent CLI sessions sharing the state dir cannot lose updates (same
// rationale as mutateDurabilityLedger).
func mutateReworkLedger(fn func(led *reworkLedger)) {
	_ = sign.WithBufferLock(reworkLedgerPath()+".lock", func() error {
		led := readReworkLedgerUnlocked()
		fn(&led)
		writeReworkLedgerUnlocked(led)
		return nil
	})
}

// clearReworkLedger drops a root's entire rework tracking — called once the
// branch has merged back to the default branch, so surviving AI lines pass to the
// durability engine and a future branch never remaps against stale ranges.
func clearReworkLedger(rootKey string) {
	mutateReworkLedger(func(led *reworkLedger) {
		delete(led.Roots, rootKey)
	})
}

// aiRangesForSeeding pulls the likely_ai new-side spans (content-free) out of the
// reconciled attribution files, keyed by path, for first-touch seeding.
func aiRangesForSeeding(files []attrFile) map[string][]durTrackedRange {
	out := map[string][]durTrackedRange{}
	for _, f := range files {
		for _, r := range f.LineRanges {
			if r.Attribution != attributionLikelyAI {
				continue
			}
			out[f.Path] = append(out[f.Path], durTrackedRange{Start: r.Start, End: r.End})
		}
	}
	return out
}

// pollReworkCommit folds ONE pre-merge working-HEAD commit into the rework
// ledger, reusing the attribution watcher's already-fetched diff + files (no
// extra git spawn): (1) remap every tracked path this commit touched — a churned
// AI span is emitted as a rework_verdict and dropped; (2) FIRST-TOUCH seed the
// AI-authored paths this commit introduces. Returns (and emits) one verdict per
// (commit, path) that had a churn.
func pollReworkCommit(session Session, root, sha, diff string, files []attrFile, nowMs int64) []event.Event {
	hunks := parseUnifiedDiffHunks(diff)
	seedable := aiRangesForSeeding(files)
	rootKey := gitWatchRootKey(root)

	var verdicts []event.Event
	mutateReworkLedger(func(led *reworkLedger) {
		tracked := led.Roots[rootKey]
		if tracked == nil {
			tracked = map[string][]durTrackedRange{}
		}
		// (1) Remap/churn every already-tracked path this commit rewrote. `remapped`
		// records paths that were tracked BEFORE this commit so step (2) never
		// re-seeds one whose ranges this commit just churned to empty (first-touch).
		remapped := map[string]bool{}
		for path, hs := range hunks {
			existing := tracked[path]
			if len(existing) == 0 {
				continue
			}
			remapped[path] = true
			surv, churned := remapTrackedRanges(existing, hs)
			if len(surv) > 0 {
				tracked[path] = surv
			} else {
				delete(tracked, path)
			}
			if len(churned) > 0 {
				verdicts = append(verdicts, buildReworkVerdict(session, root, sha, path, churned, nowMs))
			}
		}
		// (2) First-touch seed newly-introduced AI paths.
		for path, rs := range seedable {
			if remapped[path] || len(tracked[path]) > 0 {
				continue // already tracked / just churned — first-touch only
			}
			lineage := durLineageID(sha, path)
			var seeded []durTrackedRange
			for _, r := range rs {
				r.LineageID = lineage
				r.BornTsMs = nowMs
				seeded = append(seeded, r)
			}
			if len(seeded) > 0 {
				tracked[path] = seeded
			}
		}

		if len(tracked) > 0 {
			if led.Roots == nil {
				led.Roots = map[string]map[string][]durTrackedRange{}
			}
			led.Roots[rootKey] = tracked
		} else {
			delete(led.Roots, rootKey)
		}
	})

	for i := range verdicts {
		emitReworkVerdict(verdicts[i])
	}
	return verdicts
}

// reworkVerdictData is the CLOSED payload of a rework_verdict event: the commit
// that did the rework, the path, and the AI ranges it churned (old-side line
// numbers, with the age each AI span lived before being reworked). Scalars only.
type reworkVerdictData struct {
	CommitSha      string            `json:"commitSha"`
	WorkspaceKey   string            `json:"workspaceKey"`
	Path           string            `json:"path"`
	ReworkedRanges []durVerdictRange `json:"reworkedRanges"`
	MeasuredTsMs   int64             `json:"measuredTsMs"`
}

// buildReworkVerdict assembles a rework_verdict for one path. Data goes through
// eventDataMap (JSON round-trip) so the nested range array lands as
// []interface{} of map — the only shape the redaction projector's element
// allowlist can walk (a straight struct assignment ships {}).
func buildReworkVerdict(session Session, root, sha, path string, reworked []durTrackedRange, nowMs int64) event.Event {
	e := event.NewEvent("rework_verdict", session.DeviceID)
	e.Source = presenceSource
	e.DeviceID = session.DeviceID
	e.Actor = event.SystemActor()
	e.Data = eventDataMap(reworkVerdictData{
		CommitSha:      sha,
		WorkspaceKey:   workspaceKey(root),
		Path:           path,
		ReworkedRanges: toVerdictRanges(reworked, nowMs),
		MeasuredTsMs:   nowMs,
	})
	return e
}

// emitReworkVerdict funnels a verdict through the SAME sign/redact/queue path as
// every captured event, reusing the shared outbox drain singleton. Best-effort.
func emitReworkVerdict(ev event.Event) {
	if err := sign.AppendEventToLocalBuffer(&ev, false); err != nil {
		state.HookDebugf("rework verdict buffer error: %v", err)
	}
	if err := outbox.Append(ev); err != nil {
		state.HookDebugf("rework verdict queue error: %v", err)
	}
}
