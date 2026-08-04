# Durability ledger (`internal/capture/durability.go`)

The invariants live in that file's header comment — read it before changing
anything in the ledger; it is the authoritative statement and is kept current.

The one framing worth carrying in before you open it: this subsystem's worst
failure mode is **FABRICATION**, not loss. Losing a tracked line understates AI
impact; inventing one (attributing human code as AI) or inventing survival
(emitting `durable` for lines that did not survive) corrupts the only number the
product exists to report. Every tradeoff in there is resolved toward a
conservative undercount, and a change that trades an undercount for any kind of
invention is wrong even when it improves coverage.

Two fabrication classes were live in shipped code and are now pinned by
`durability_fabrication_test.go`: a path that leaves the ledger re-arming
first-touch seeding, and a rename stranding spans at a dead path. Both were
reachable through ordinary git usage, so treat any new path that removes or
relocates a ledger entry as suspect until it has a test.

**The pre-merge rework ledger (`rework.go`) is a SECOND ledger holding the SAME
invariants**, and both fabrication classes were reachable through both. Fix them
together or the fix is half a fix.

Two things about it that are easy to get wrong, both learned the expensive way:

- **A seed tombstone blocks INFERENCE, not seeding.** The unsafe act was never
  "seeding a path twice" — it was seeding a path as AI because we had merely seen
  it before. So a tombstone stores the AI-write evidence already spent on that
  path, and only a *strictly newer* per-path AI write stamp — an agent
  demonstrably wrote the file again — re-authorizes it; a stamp that merely
  differs does not. Blocking outright silences the canonical agent loop — write,
  rewrite, rewrite again — which is the very thing rework measures. Both failure
  directions are pinned by tests; do not "simplify" the token comparison back into
  a boolean. **Durability's tombstone holds the identical rule** (its own
  `durabilitySeedAuthorized`), and the two halves that took a second pass are worth
  not re-deriving: refreshing the mark on every BLOCKED attempt made live AI work
  the thing that kept a path buried — a file the agent kept editing went unreported
  for good after its first `durable` harvest — and a rename must carry the LATER of
  the source's spent stamp and the destination's own, since a mark holding only the
  source's is read against a different path's clock and the agent that renamed the
  file usually recorded the new name too. Durability's marks are pruned by EVIDENCE
  rather than by a TTL, and the pruner must ask the SEED GATE'S OWN QUESTION —
  presence in the AI-paths ledger, never a second expression that merely looks
  compatible — but over EVERY checkout of the repository, a WIDER domain than the
  gate itself is authorized against (the two-view rule below, stated in full in
  `durability.go`'s header). Judging a mark by its write stamp is what re-opens
  the hole: a ledger written before per-path stamps carries its paths with a 0
  stamp for the full 7-day TTL, so every tombstone on a deployed install is
  deleted while the gate is still consulting the path.
- **Its state is scoped to a BRANCH, and `reworkLedger.Branches` is what enforces
  that.** The clear on `scope == scopeDefault` alone is not a bound: `git switch
  -c` off a feature branch and a per-branch worktree never stand on the default
  branch at all.

- **Two ledgers with different keys lose data where they meet.** Rework state is
  per-ROOT and expires with its branch; the attributed-commits ledger is keyed by
  SHA ALONE and shared across roots. Each is right alone, so a root adopting a
  branch another worktree (or an earlier checkout) already attributed starts
  empty and has every rebuilding commit skipped — the branch reads as holding no
  AI work. `replayReworkForAdoptedCommit` rebuilds that state silently, and it
  runs ONLY while the root still OWES the rebuild (`reworkLedger.Adopting`, set by
  adoption and cleared the poll the root's cursor reaches its head): cursor
  recovery re-surfaces skipped commits with no branch change, and re-folding one
  there re-applies its insertion hunk and slides live spans out of the file's real
  line space. That obligation is ON DISK because the range it covers is not
  bounded by one poll — `pollGitWatch`'s shared budget defers a whole root and its
  burst clamp splits a branch across polls, and every poll after the first sees an
  unchanged branch. A per-poll flag therefore expired mid-range, which lost the
  deferred branch outright and left a clamped one holding spans positioned
  mid-branch: the next live commit remapped those into a verdict about lines
  nobody wrote. `pollGitWatch`'s `drained` map is what says a root drained.

- **A NEW ROOT is a third way into that same loss, and it is one-shot.** A fresh
  `git worktree add` is a new `gitWatchRootKey`, so `pollGitWatch` cold-starts it:
  the cursor goes straight to head and the branch's commits are never surfaced,
  while adoption fires anyway and immediately finishes a rebuild that rebuilt
  nothing. `replayReworkForColdStartBranch` folds `default..head` for state,
  gated on this device ALREADY holding attribution for a commit in the range —
  that gate, not the cold-start cursor, is what keeps a fresh install from
  importing history it never measured. It takes the range WHOLE or not at all
  (`gitBranchCommitsSinceDefault`): unlike `clampCommitBurst`, there is no later
  poll to drain a remainder, so a partial replay starts mid-branch and both cut
  ends FABRICATE — the older leaves spans a commit behind head, the newer makes
  the first replayed commit look like a first touch and seeds a human's added
  lines on an AI-touched path as AI (measured, not predicted).

  Three things decide WHERE the rebuilt spans land, and each was a live
  mispositioning before it was pinned. **The range is resolved against the head
  SHA `pollGitWatch` baselined the cursor to** — `pollGitWatch`'s `coldStart` map
  carries that SHA, not a bool — because re-reading `HEAD` a whole poll later lets
  a commit made in between be folded here AND detected as new next poll, folding
  its hunks twice. **`--first-parent`**, because `gitCommitRawDiff` reads every
  commit with `-m --first-parent`, so a merge's diff already carries the
  second-parent side; folding that side's own commits too applies the same hunks
  twice, in a sibling branch's coordinates. **`--topo-order`**, because rev-list's
  default is reverse COMMIT-DATE order, which git does not promise is topological:
  one skewed committer clock sorts a parent past its own descendants and the
  reversed fold applies it after its child. **And the caller gates on adoption
  having fired THIS poll (`justAdopted`), never on the persisted `Adopting`
  obligation** — that obligation deliberately outlives its poll, so a root whose
  cursor is lost while its ledger survives would otherwise replay the whole range
  over spans it already tracks. The same commit's hunks are never folded twice
  into the same ledger.

- **ATTRIBUTION AND THE REWORK FOLD TAKE DELIBERATELY DIFFERENT RANGES, and
  `gitNewCommits`' `foldable` return is that split.** `gitCommitRawDiff` reads every
  commit with `-m --first-parent`, so a merge's diff already carries the
  second-parent side; the live range (`rev-list <cursor>..HEAD`) returns the merge
  AND that side's own commits, so folding both applied each merged-in edit twice —
  once in a sibling's coordinate space, once in the merge's — and slid tracked
  spans by an offset already applied. So the fold walks the FIRST-PARENT chain
  (`gitFirstParentSet`) while EMISSION keeps the full range: a commit reachable
  only through a second parent is still attributed, because whether merged-in work
  counts as AI-written is a product question that was deliberately not answered
  here. Where the ranges disagree the ledger can only LOSE state — such a commit
  no longer seeds its own ranges, and a replay backed solely by one is declined
  by the attributed-in-range gate — never invent it.

  **"Not on the chain" and "the probe could not run" are DIFFERENT FACTS, and
  collapsing them into one empty set INFLATES.** An earlier revision of this file
  claimed a failed `rev-list` returning an empty set was safe because folding
  nothing under-reports. That was wrong, and writing it down was worse than the
  bug, because it is what would stop the next reader checking: an empty subset
  leaves every commit unfoldable while `pollGitWatchWorkspace` still attributes
  each one and `recordAttributedCommits` writes them all down, so the tracked
  spans keep coordinates the unfolded hunks have already moved and the next
  rewrite emits a verdict over lines nobody wrote. `commitsWithFoldableChain` is
  the one door both values come through, and a failed probe returns `ok=false`
  from `gitNewCommits` — cursor untouched, attribution and fold both retried next
  poll. It also takes ONE revision, never rev-list flags, so the probe cannot be
  bounded differently from the list it describes: the gc'd-cursor recovery path
  bounds its own list with `-n` and probes unbounded head, because a chain commit
  falling inside the all-parents window and outside a `-n`-bounded chain window
  reads as "not on the chain" and is the same collapse by another route.

- **BOTH LEDGERS FOLD A MERGE EXACTLY ONCE, AND THE INVARIANT IS NOW STRUCTURAL
  AT EVERY GATE IN THAT LOOP.** The rule both halves enforce: *a ledger's tracked
  spans are addressed in the line space the commits it has FOLDED compose to, so
  the code that RECORDS a commit and the code that FOLDS it must never disagree.*
  The two ledgers' numbers therefore reconcile on a history containing merges.

  - **DURABILITY narrows in its own ENUMERATOR** — `gitNewDefaultBranchCommits`,
    NOT a filter at the fold. Read that header before touching it. The cursor is
    the reason: `pollDurabilityCommit` advances it per commit inside that
    commit's ledger transaction, so a filter at the fold leaves it nowhere safe.
    Advance it on a skipped commit and it parks OFF the chain, where
    `cursor..tip` re-includes commits already folded; never advance it and a
    batch holding no chain commit never moves it at all — `clampCommitBurst`
    keeps the OLDEST cap commits, so merging a branch longer than the cap fills
    one entirely with second-parent commits and durability stalls for that root
    forever. Narrowing at the enumerator DISSOLVES the question rather than
    answering it: every commit returned is one that gets folded, so **the cursor
    only ever lands on a commit it folded**, and the root always drains.
  - **REWORK's remaining gate is the scope switch's `default:` arm.** A scope
    that folds nothing RELEASES the spans it cannot maintain, gated on the poll
    having surfaced commits at all — a bare `git checkout --detach` and a bisect
    walking ancestors of the cursor keep their tracking BECAUSE their range is
    empty; detaching onto a ref off that cursor's ancestry releases anyway, an
    undercount and never a fabrication. It is `default:` rather than
    `case scopeUnknown:` so a fourth `branchScope` added later cannot silently
    reopen the hole. `releaseReworkSpans` deliberately is NOT
    `clearReworkLedger`: it keeps the seed tombstones and tombstones every path
    it releases, because dropping them makes an already-churned path a FIRST
    TOUCH again and its lingering ai-paths presence would seed a human's commit
    as fresh AI — trading this fabrication for the one PR #128 closed.
  - **EMISSION is untouched by both.** A commit reachable only through a merge's
    second parent is still attributed exactly once; whether merged-in work counts
    as AI-written stays a product question nobody has answered.

  **Still open, and NAMED here rather than left to be rediscovered: a BACKWARD
  move of a tracked ref advances a cursor over spans nothing folded.**
  `rev-list A..B` is empty when B is an ancestor of A — a `git reset --hard`
  back, a force-push — and both loops read that as "nothing new" and move the
  cursor to the new tip while the spans keep the old tip's coordinates. It is
  PRE-EXISTING and unchanged by the fixes above (verified: the first-parent range
  is empty exactly when the all-parents range is, so the narrowing added no new
  empty-result path), and it hits BOTH ledgers identically, so they still agree
  with each other. Closing it needs `pollGitWatch` to tell "nothing moved" apart
  from "moved backwards" — a new signal out of that function, not an edit.

  **The rest of that sweep came back clean, and the bound was ESTABLISHED rather
  than assumed** — do not redo it. Four sites were checked for the same signature
  (an empty or narrowed result treated as harmless while commits are still
  attributed and recorded): `gitBranchCommitsSinceDefault` returning nil records
  no attribution at all, so it is a pure documented undercount (the cold-start
  cursor is already at head); `commitsWithFoldableChain`'s `len(shas)==0` case
  has no commits to disagree about; `replayReworkForAdoptedCommit`'s empty-diff
  early return runs only on commits already attributed and carries no hunks to
  lose; and `attributeAndReworkCommit`'s `!ok` branch skips the fold only when
  the diff is empty, which has nothing to shift.

Rename handling has an extra trap rework does not share with durability: a pure
rename produces no `@@` hunks, so `commitAttributionFromDiff` reports `ok=false`
and the commit would never reach `pollReworkCommit` at all. It returns the raw
diff on that path specifically so renames still land.

**A COMMIT BELONGS TO THE REPOSITORY, NOT TO A CHECKOUT OF IT, and that is why
`ledgerScope` carries the repo's other worktrees.** The ai-paths ledger records
a path relative to whichever checkout the agent edited in, so a lookup keyed on
the polled checkout alone read every commit made in a SIBLING worktree as
`unknown` — a silent under-count of `commit_attribution` itself, and the
ceiling on both cross-checkout replays above (they replayed the right commits
and found no AI ranges to seed). `resolveLedgerScope` now appends one alt scope
per `git worktree list` entry and `ledgerLookup` falls through them, own
checkout FIRST so a single-checkout machine is byte-for-byte unchanged and a
two-worktree tie is deterministic.

**The widening is over CHECKOUTS, never over PATHS — and only over checkouts on
THIS COMMIT'S OWN LINE OF HISTORY.** Only this repository's worktrees are
consulted, so another repo, or a CLONE of the same upstream, cannot contribute
evidence — a clone has its own object store and its own worktree list, and that
under-count is deliberate. The committed path must still match exactly.

A bare repo-relative match across checkouts FABRICATES, because worktrees are
how one repository holds several branches at once: an agent writes
`internal/x.go` in the feature worktree, a human hand-edits `internal/x.go` on
the default branch inside the 7-day TTL and commits it there, and the human's
commit inherits the agent's session. So every alt is gated: **a sibling's
evidence counts ONLY when that checkout's HEAD REACHES the commit** — the commit
is an ancestor-or-self of its HEAD, i.e. that checkout holds it. Where it does
not, the lookup MISSES rather than falling through to the bare path.

**ONE DIRECTION ONLY — "the commit DESCENDS from the sibling's HEAD" is not
accepted, and an earlier revision that accepted it gated nothing.** `git worktree
add -b feat` leaves the new checkout parked on the branch point and evidence is
recorded when the agent WRITES, with no commit of its own — so every later commit
on every branch descends from that HEAD. That is the ORDINARY state of a worktree,
not a corner case, and in it the descends-from direction hands the agent's
evidence to a human's commit on the default branch.
`TestCommitAttributionDoesNotCrossToASiblingTheCommitMerelyDescendsFrom` pins it
(absence of attribution, of a session, and of a durability span).

**What that deliberately does NOT recover**: evidence held by a checkout that
does not hold the commit — the agent's worktree switched branches, was reset, or
never committed what it wrote. That reads `unknown`. It is a conservative
under-count, on purpose, and a wrong number outranks a missing one. The residual
that DOES remain is the path-level one `reconcileCommitAttribution` note 2
already documents — a file AI-touched then human-edited inside the 7-day TTL
reads `likely_ai` — now reachable from a sibling that holds the commit rather
than from the polled directory alone. It is NOT "the same granularity" as the
one-directory residual: the blast radius is every checkout holding that commit,
which is why the lineage gate is the thing holding it down.

**THE GATE AND THE TOMBSTONE BOOKKEEPING READ THE SAME MARKS THROUGH TWO
DIFFERENT VIEWS, and collapsing them is a live defect in both ledgers.**
`resolveLedgerScope` (gated) is what may AUTHORIZE a seed. Everything that can
only ever SUPPRESS one takes `resolveLedgerScopeAllCheckouts` (every checkout,
ungated): `harvestDurable`, which ages spans out on a clock and holds no commit
to gate against, **and the tombstone bookkeeping in `pollDurabilityCommit` and
`foldReworkCommit`** — `pruneSeedTombstones`, and the stamp a departing path is
tombstoned at. Under the narrow view a sibling moving off the commit PRUNES a
live tombstone (re-arming first-touch seeding) and tombstones a departing path at
0, so the write already spent on it clears the strictly-newer gate and seeds it
again. Both are pinned by tests, and both tombstone writers are additionally
MONOTONIC — a mark may only ever rise. `durability.go`'s header states the
two-view rule in full.

**COST: one `git rev-list` per sibling per commit-loop, per poll — NOT per
commit.** `siblingLineage` asks each sibling once, for the poll's whole range,
and every gate answer is then read from memory; a poll spends 2N reads per root
with N siblings (the attribution loop and the durability loop each build one),
plus one per cold-start replay, and that number does not grow with the commit
count. The earlier per-commit `git merge-base --is-ancestor` shape cost up to
`gitWatchMaxCommitsPerPollTotal` × 3 call sites × N siblings × 2 processes —
~1,200N per burst poll, tens of seconds of subprocess time inside a 60s interval,
landing exactly on the bursts (cold start, adoption replay, a large pull) the
recovery paths exist for.

`repoHasLinkedWorktrees` short-circuits, stat-only, before any spawn or memo, so
a repo that never had `git worktree add` run against it costs nothing AND picks
its first worktree up immediately. Only a repo that already has one pays the
memoized `git worktree list` (≤1 spawn per root per poll) and the reachability
reads. `resolveLedgerScope` runs once per COMMIT in three places, so both the
enumeration and the mapping of siblings onto the ledger path space are memoized
on one entry (`siblingLedgerScopes`) — an unmemoized spawn, or a re-resolved
scope, would scale with commits × worktrees.

**Any test here must use a REAL second directory.**
`TestReworkAdoptionRebuildsSpansAttributedByAnotherWorktree` simulates the
second worktree inside ONE directory, which is exactly what let this defect
through — one directory is one ledger key, so the divergence never appears.
`commit_attribution_worktree_test.go` uses `git worktree add` throughout, and
two tests there are the bound that keeps the widening honest — both with real AI
evidence at the same relative path, both asserting the ABSENCE of attribution, of
a session, and of a durability span:
`TestCommitAttributionDoesNotCrossADivergentSiblingBranch` (sibling on another
branch) and `TestCommitAttributionDoesNotCrossToASiblingTheCommitMerelyDescendsFrom`
(sibling parked on the branch point with no commit of its own).

