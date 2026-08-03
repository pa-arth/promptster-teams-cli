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
// churn/shift math, the same seeding discipline, and the same single-locked
// read-modify-write ledger. The ONLY differences from durability are:
//   - it advances on WORKING HEAD, gated to commits made while the branch is
//     ahead of the default branch (pre-merge) — see pollGitWatchWorkspace;
//   - a churned tracked range emits a rework_verdict immediately (there is no
//     maturity window — a rewrite IS the event);
//   - it reuses the attribution watcher's ALREADY-fetched `git show` diff + files
//     (no extra spawn), so a pre-merge commit stays one `git show`;
//   - it carries branch identity, because it is branch-scoped state (see
//     reworkLedger.Branches), and a root that adopts a branch another root
//     already attributed OWES a state-only replay of those commits (see
//     reworkLedger.Adopting and replayReworkForAdoptedCommit).
//
// SEEDING: INFERENCE IS BLOCKED, EVIDENCE IS NOT. The unsafe thing was never
// "seeding a path twice" — it was seeding a path as AI merely BECAUSE WE HAD
// SEEN IT BEFORE. The AI-paths ledger is path-scoped with a 7-day TTL, so once a
// path has been seeded and churned out, its lingering presence in that ledger
// says nothing about who wrote the NEXT commit to it: taking presence as
// authority re-attributes a human's rewrite as fresh AI, which is the inflation
// the privacy rules forbid (unknown is NEVER promoted to AI).
//
// So a path that has left the ledger carries a SEED TOMBSTONE recording the
// AI-WRITE STAMP that was already consumed for it, and re-seeding is authorized
// only by a STRICTLY NEWER stamp (reworkSeedAuthorized). Mere presence never
// re-authorizes anything, and neither does a stamp that merely CHANGED — only
// forward motion of a per-path write stamp is proof that an agent wrote the file
// again, which is precisely the class of evidence that authorizes seeding an
// unseen path in the first place.
//
// Both directions of getting that boundary wrong are failures, and they are not
// symmetric in kind but are both disqualifying: too permissive fabricates (human
// code reported as AI), too restrictive silences the canonical agent loop (write,
// rewrite, rewrite again — every iteration after the first invisible), which
// presents under-reporting as measurement. Both are pinned by tests.
//
// PRIVACY: identical to durability — only integer ranges, an age, and a
// `sha:path` lineage handle leave; never bytes, diffs, or content.

const reworkLedgerVersion = 1

// reworkLedger is the on-disk pre-merge rework state: rootKey → path → the AI
// spans currently tracked on the working branch. It needs no cursor of its own —
// it piggybacks on the attribution watcher's working-HEAD detection.
type reworkLedger struct {
	V     int                                     `json:"v"`
	Roots map[string]map[string][]durTrackedRange `json:"roots"`
	// Seeded is rootKey → path → the AI-WRITE STAMP that has ALREADY been consumed
	// for that path: the SEED TOMBSTONES. An entry does not mean "this path is
	// dead to us"; it means "presence alone proves nothing here anymore, because
	// this is the evidence we already spent". Seeding is re-authorized only by a
	// STRICTLY NEWER stamp — an agent demonstrably wrote the file again — never by
	// a stamp that merely differs. See reworkSeedAuthorized.
	//
	// Additive — an older ledger reads it as nil, which every reader tolerates.
	// Do NOT bump reworkLedgerVersion to add a field (same reasoning as the
	// durability ledger's).
	Seeded map[string]map[string]int64 `json:"seeded,omitempty"`
	// Branches is rootKey → the branch this root's rework state describes, and it
	// is what BOUNDS the whole ledger.
	//
	// The bound used to be stated as "clearReworkLedger wipes it when the branch
	// merges back", which the code did not provide: that clear fires only on
	// scope == scopeDefault (pollGitWatchWorkspace), so `git switch -c next-thing`
	// straight off a feature branch, or a per-branch worktree that never checks
	// out the default branch, never reached it — and the state silently outlived
	// the branch it belonged to. Recording the branch makes the clear fire on the
	// event that actually invalidates the state (the branch changing), not on the
	// hope that someone eventually stands on the default branch.
	//
	// What that buys, stated honestly: state lives at most one branch, so it is
	// bounded by the paths one branch touches rather than by repo lifetime. A
	// worktree parked on one long-lived branch keeps that branch's marks for as
	// long as the branch exists, which is correct — they ARE that branch's state.
	Branches map[string]string `json:"branches,omitempty"`
	// Adopting is rootKey → true while an adoption still OWES its replay: the
	// root's spans were dropped for the incoming branch and the already-attributed
	// commits that rebuild them have not all been folded back yet.
	//
	// It is ON DISK because the range it authorizes is not bounded by one poll.
	// pollGitWatch spends a per-poll budget shared across roots and clamps each
	// root's burst, so an adopted branch's commits routinely arrive over several
	// polls — and every poll after the first sees an UNCHANGED branch, which is
	// indistinguishable from steady state without this marker. Deriving the
	// authorization from "this poll changed the branch" therefore expired it
	// mid-range, leaving the rest of the branch skipped with nothing to replay it.
	//
	// It is cleared the moment the root's cursor reaches its head, and that
	// promptness is load-bearing in the other direction: while it stands, a
	// cursor-recovery batch would be folded into live state, re-applying hunks
	// that are already in the ledger. Both directions are pinned by tests.
	//
	// Additive, same as Seeded — an older ledger reads it as nil.
	Adopting map[string]bool `json:"adopting,omitempty"`
}

// tombstoneReworkSeededPath records the AI-write stamp already consumed for a
// path, so no later read of that same write can re-authorize seeding. A zero
// stamp is meaningful: it records "there was no per-write AI evidence here",
// which only inference could have seeded, and inference stays blocked.
func tombstoneReworkSeededPath(led *reworkLedger, rootKey, path string, writeMs int64) {
	if led.Seeded == nil {
		led.Seeded = map[string]map[string]int64{}
	}
	if led.Seeded[rootKey] == nil {
		led.Seeded[rootKey] = map[string]int64{}
	}
	led.Seeded[rootKey][path] = writeMs
}

// reworkSeedAuthorized reports whether this commit may seed path, given the
// AI-write stamp backing it right now.
//
// No tombstone → yes, this is a genuine first touch. Tombstone → yes ONLY if an
// agent has written the path since the stamp we already spent on it. That is the
// entire narrowing: PATH-LEVEL INFERENCE can never seed a tombstoned path, while
// a demonstrable fresh agent write always can.
//
// STRICTLY NEWER, not merely different, and the difference is the whole
// invariant. A stamp that changed is not proof that anything was written — it can
// change because the evidence for this path was never per-path to begin with, or
// because a different session became the one we read it from. Only forward motion
// of a per-path write stamp is a write. So a backwards move, a tie, a TTL
// eviction, and "no per-write evidence at all" (0) every one read as blocked,
// which is the undercount-never-inflation direction this file commits to.
func reworkSeedAuthorized(led *reworkLedger, rootKey, path string, writeMs int64) bool {
	consumed, tombstoned := led.Seeded[rootKey][path]
	if !tombstoned {
		return true
	}
	return writeMs != 0 && writeMs > consumed
}

// reworkSeedEvidence is this poll's view of per-path AI-WRITE evidence — as
// opposed to the mere path-presence that reconcileCommitAttribution uses to stamp
// likely_ai.
//
// The distinction is the point, and it has to live here because attrFile does
// not carry it: reconcileCommitAttribution marks a file likely_ai from path
// presence OR from a bash-mtime window, and BOTH are inference — neither says an
// agent wrote these particular lines in this particular commit.
//
// It is a snapshot taken once per commit rather than a per-path lookup because
// the ai-paths ledger has its OWN lock: reading it lazily from inside the rework
// ledger's read-modify-write would nest two locks.
type reworkSeedEvidence struct {
	scope ledgerScope
	marks map[string]aiPathMark
}

func newReworkSeedEvidence(root, taskRoot string) reworkSeedEvidence {
	scope := resolveLedgerScope(root, taskRoot)
	return reworkSeedEvidence{scope: scope, marks: readAiPathMarks(scope.aiKey)}
}

// writeStampFor is when an agent last WROTE path — a value that advances when,
// and only when, the file is written again. 0 means no per-write evidence exists
// for the path at all (never recorded, recorded before per-path stamps existed,
// or aged out of the ai-paths TTL), which reads as inference and is refused on a
// tombstoned path.
//
// The path is translated through the ledger scope, exactly as attribution does,
// so a repo discovered under the daemon's HOME workspace looks its evidence up
// under the same key it was recorded with.
func (e reworkSeedEvidence) writeStampFor(path string) int64 {
	return e.marks[e.scope.ledgerPath(path)].WriteMs
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

// dropReworkRoot removes every trace of a root from an already-open ledger:
// tracked spans, seed tombstones, and the recorded branch. All three are that
// branch's state and expire together — leaving the tombstones behind would block
// seeding on the next branch, and leaving the branch behind would suppress the
// change detection that clears it.
func dropReworkRoot(led *reworkLedger, rootKey string) {
	delete(led.Roots, rootKey)
	delete(led.Seeded, rootKey)
	delete(led.Branches, rootKey)
	delete(led.Adopting, rootKey)
	if len(led.Seeded) == 0 {
		led.Seeded = nil
	}
	if len(led.Branches) == 0 {
		led.Branches = nil
	}
	if len(led.Adopting) == 0 {
		led.Adopting = nil
	}
}

// clearReworkLedger drops a root's entire rework tracking — called once the
// branch has merged back to the default branch, so surviving AI lines pass to the
// durability engine and a future branch never remaps against stale ranges.
func clearReworkLedger(rootKey string) {
	mutateReworkLedger(func(led *reworkLedger) {
		dropReworkRoot(led, rootKey)
	})
}

// reworkLedgerHasRoot reports whether a root has any rework state left to clear —
// tracked spans, seed tombstones, or a recorded branch. Tombstones outlive the
// tracked map (a fully churned root deletes its Roots entry but keeps its marks),
// so a Roots-only check would leave them stranded and permanently block seeding
// on the next branch.
func reworkLedgerHasRoot(rootKey string) bool {
	led := loadReworkLedger()
	if _, ok := led.Roots[rootKey]; ok {
		return true
	}
	if _, ok := led.Seeded[rootKey]; ok {
		return true
	}
	if _, ok := led.Adopting[rootKey]; ok {
		return true
	}
	_, ok := led.Branches[rootKey]
	return ok
}

// reworkLedgerBranchState returns the branch a root's rework state describes and
// whether that root still owes an adoption replay, from ONE ledger read — the
// poll loop needs both together on every pre-merge root and reading them
// separately would double the locked reads per poll.
func reworkLedgerBranchState(rootKey string) (branch string, adopting bool) {
	led := loadReworkLedger()
	return led.Branches[rootKey], led.Adopting[rootKey]
}

// finishReworkAdoption clears the replay marker, which is what returns the root
// to the ordinary rule that a detected-but-already-attributed commit is skipped
// outright. Called only once the root's cursor has reached its head, so no
// commit of the adopted branch is still owed a fold.
func finishReworkAdoption(rootKey string) {
	mutateReworkLedger(func(led *reworkLedger) {
		if _, ok := led.Adopting[rootKey]; !ok {
			return
		}
		delete(led.Adopting, rootKey)
		if len(led.Adopting) == 0 {
			led.Adopting = nil
		}
	})
}

// adoptReworkBranch binds a root's rework state to branch, dropping whatever it
// held for a DIFFERENT branch first. This is what actually bounds the ledger: the
// state expires when the branch it describes stops being checked out, which is
// the event that invalidates it, rather than when someone happens to stand on the
// default branch (the trigger the old bound assumed and `git switch -c` skips).
//
// It also raises the replay marker, because the drop it just performed is
// exactly what makes the replay necessary. The marker is what carries that
// obligation past this poll — see reworkLedger.Adopting.
func adoptReworkBranch(rootKey, branch string) {
	mutateReworkLedger(func(led *reworkLedger) {
		if led.Branches[rootKey] == branch {
			return
		}
		dropReworkRoot(led, rootKey)
		if led.Branches == nil {
			led.Branches = map[string]string{}
		}
		led.Branches[rootKey] = branch
		if led.Adopting == nil {
			led.Adopting = map[string]bool{}
		}
		led.Adopting[rootKey] = true
	})
}

// applyReworkRenames carries a commit's renamed paths across the ledger, mutating
// tracked in place, BEFORE the hunk remap — the same ordering and the same reason
// as applyDurabilityRenames: git keys a rename's hunks under the NEW path while
// their old-side line numbers still address the OLD file, so a span carried first
// lands already in the right line space and rename+edit needs no special math.
//
// Stranding a span was never merely a loss here. A pure rename emits no hunks
// under the tracked path, so the span sat at a dead path; recreating a file at
// that path (an ordinary split-a-file refactor) let a pure INSERTION shift the
// stale span into the new file's line space, and the next edit churned it into a
// rework_verdict over lines the AI never wrote.
//
// LineageID and BornTsMs ride along untouched — a rename is not a rewrite — and
// the seed tombstone follows the file, or renaming a churned path would hand the
// same content a fresh unmarked name.
func applyReworkRenames(led *reworkLedger, rootKey string, tracked map[string][]durTrackedRange, renames map[string]string, evidence reworkSeedEvidence) {
	if len(renames) == 0 {
		return
	}
	type move struct {
		to    string
		spans []durTrackedRange
	}
	// Every source is read (and removed) before any destination is written, so a
	// chain landing in one commit (a→b, b→c) cannot clobber itself on Go's random
	// map order.
	var moves []move
	for from, to := range renames {
		consumed, wasTombstoned := led.Seeded[rootKey][from]
		if wasTombstoned {
			// The mark follows the content: the destination inherits the evidence
			// already spent, so a churned-then-renamed path cannot be re-seeded from
			// stale presence under its new name.
			//
			// The DESTINATION's own evidence is spent by the same act, and taking the
			// later of the two is what makes the carry mean anything: stamps are
			// per-path, so a mark holding only the source's stamp is compared against a
			// different path's clock, and the agent that renamed the file has usually
			// recorded the new name too — which would clear the very gate it was just
			// handed. Only a write NEWER than the rename re-authorizes.
			tombstoneReworkSeededPath(led, rootKey, to, max(consumed, evidence.writeStampFor(to)))
		}
		spans := tracked[from]
		if len(spans) == 0 {
			continue
		}
		moves = append(moves, move{to: to, spans: spans})
		delete(tracked, from)
		// The source is tombstoned with the evidence current NOW, not with an empty
		// token: a file later recreated at this path is a different file, and an
		// empty mark would let the departed file's still-live path evidence seed it.
		// Never BELOW a mark it already carried, or the move would loosen the gate.
		tombstoneReworkSeededPath(led, rootKey, from, max(consumed, evidence.writeStampFor(from)))
	}
	for _, m := range moves {
		existing := tracked[m.to]
		if len(existing) == 0 {
			tracked[m.to] = m.spans
			continue
		}
		// Real git emits modify+delete rather than a rename when the destination
		// already exists, so this is unreachable through its output; it merges
		// rather than drops because a dropped span is the stranded-lineage
		// fabrication all over again. The destination wins any overlapping line.
		merged := expandRanges(m.spans)
		for ln, meta := range expandRanges(existing) {
			merged[ln] = meta
		}
		tracked[m.to] = coalesceLines(merged)
	}
}

// aiRangesForSeeding pulls the likely_ai new-side spans (content-free) out of the
// reconciled attribution files, keyed by path, as the candidate spans for
// seeding. Whether a candidate may actually seed is decided later, by
// reworkSeedAuthorized — this is path presence, not per-write evidence.
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
// extra git spawn): (0) carry renamed paths across; (1) remap every tracked path
// this commit touched — a churned AI span is emitted as a rework_verdict and
// dropped; (2) seed the AI-authored paths this commit introduces, subject to the
// evidence rule in the file header. Returns (and emits) one verdict per
// (commit, path) that had a churn.
func pollReworkCommit(session Session, root, sha, diff string, files []attrFile, nowMs int64) []event.Event {
	verdicts := foldReworkCommit(session, root, sha, diff, files, nowMs)
	for i := range verdicts {
		emitReworkVerdict(verdicts[i])
	}
	return verdicts
}

// foldReworkCommit is pollReworkCommit's ledger half, split out so a commit can
// be folded for its STATE without emitting anything. It returns the verdicts the
// commit produced and emits none of them; pollReworkCommit is the emitting entry
// point and stays the only caller that ships events.
//
// The silent caller is replayReworkForAdoptedCommit. Its commits were already
// attributed — by another worktree, or by this root before a branch switch — so
// their verdicts have already been emitted once. Re-emitting them is exactly the
// double-count the attribution skip exists to prevent, which is why the split is
// at emission rather than at the ledger write.
func foldReworkCommit(session Session, root, sha, diff string, files []attrFile, nowMs int64) []event.Event {
	hunks := parseUnifiedDiffHunks(diff)
	renames := parseUnifiedDiffRenames(diff)
	seedable := aiRangesForSeeding(files)
	if len(hunks) == 0 && len(renames) == 0 && len(seedable) == 0 {
		return nil // nothing to remap, move or seed — don't take the ledger lock
	}
	// Resolved BEFORE the ledger lock, matching pollDurabilityCommit: the ai-paths
	// ledger has its own lock and the rework ledger's read-modify-write must never
	// nest another one.
	evidence := newReworkSeedEvidence(root, session.TaskRoot)
	rootKey := gitWatchRootKey(root)

	var verdicts []event.Event
	mutateReworkLedger(func(led *reworkLedger) {
		tracked := led.Roots[rootKey]
		if tracked == nil {
			tracked = map[string][]durTrackedRange{}
		}
		// (0) Renames first — see applyReworkRenames for why the order is load-bearing.
		applyReworkRenames(led, rootKey, tracked, renames, evidence)
		// (1) Remap/churn every already-tracked path this commit rewrote. `remapped`
		// records paths that were tracked BEFORE this commit so step (2) never
		// re-seeds one whose ranges this commit just churned to empty.
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
				// The path leaves the ledger, so record which evidence has been spent
				// on it. Without that, step (2) re-arms on a LATER commit — `remapped`
				// only guards the same commit — and the next purely-human commit to
				// this path is seeded as fresh AI off the stale path evidence.
				delete(tracked, path)
				tombstoneReworkSeededPath(led, rootKey, path, evidence.writeStampFor(path))
			}
			if len(churned) > 0 {
				verdicts = append(verdicts, buildReworkVerdict(session, root, sha, path, churned, nowMs))
			}
		}
		// (2) Seed the AI paths this commit introduces. A tombstoned path is seeded
		// only on evidence of a fresh agent write, never on path presence alone.
		for path, rs := range seedable {
			if remapped[path] || len(tracked[path]) > 0 {
				continue
			}
			if !reworkSeedAuthorized(led, rootKey, path, evidence.writeStampFor(path)) {
				continue
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
				// Spend the evidence: the same token must not authorize a second
				// re-entry once these spans churn back out.
				if _, tombstoned := led.Seeded[rootKey][path]; tombstoned {
					tombstoneReworkSeededPath(led, rootKey, path, evidence.writeStampFor(path))
				}
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

	return verdicts
}

// replayReworkForAdoptedCommit rebuilds one already-attributed commit's rework
// STATE, emitting nothing.
//
// Why it has to exist. Rework state is per-ROOT and expires with the branch it
// describes; the attributed-commits ledger is keyed by SHA ALONE and shared
// across roots. Both are right on their own, and together they lose data: a root
// that adopts a branch starts with no spans, and every commit that would rebuild
// them is skipped as already-attributed before it can. The branch then looks like
// it contains no AI work at all, so later rewrites of its AI lines emit no
// rework_verdict — silent under-reporting, reachable through two ordinary moves
// (park on the default branch and come back, or run several worktrees on one
// repository).
//
// The fix deliberately does NOT relax the skip. Attribution stays skipped, no
// commit_attribution is re-emitted, and no fingerprints are re-recorded — the
// commit really was accounted for. Only the ledger state a later commit needs in
// order to recognise its own churn is rebuilt, and only silently.
//
// It is called ONLY while the root OWES an adoption replay (reworkLedger.
// Adopting), which is what makes it safe to fold a commit the ledger may have
// seen before: adoption emptied this root's state, and the range being replayed
// runs forward from that empty ledger to the root's head, so nothing is applied
// twice. Without that guard, gitNewCommits' cursor-recovery path — which
// re-surfaces the newest commits wholesale after a history rewrite — would
// re-fold commits whose spans are already tracked and remap surviving ranges a
// second time.
//
// The marker outlives the adopting poll deliberately: the budget and burst caps
// hand an adopted branch's commits over several polls, and an authorization that
// died with the first poll left the remainder skipped — the branch either
// silently empty, or worse, holding spans positioned mid-branch for the next
// live commit to remap into lines nobody wrote.
//
// Costs one `git show` per replayed commit, on adoption polls only; a steady-state
// poll on an unchanged branch replays nothing.
func replayReworkForAdoptedCommit(session Session, root, sha string, nowMs int64) {
	diff, files, _, ok := commitAttributionFromDiff(root, session.TaskRoot, sha)
	if diff == "" {
		return // nothing this commit can contribute (merge commit, empty diff)
	}
	if !ok {
		// Mirrors attributeAndReworkCommit's !ok branch: a pure rename and a
		// delete-only commit both still move or churn tracked spans, and neither
		// may seed — removing a file is not evidence that anyone wrote anything.
		files = nil
	}
	foldReworkCommit(session, root, sha, diff, files, nowMs)
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
