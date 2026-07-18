package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// Squash-merge attribution transfer (the long pole).
//
// A squash-merge collapses a feature branch's commits into ONE new commit on the
// default branch with NO ancestry to the branch commits. The default-branch
// durability poller sees it as brand-new, and its diff is the union of the
// branch's changes with no per-line AI/human signal. To keep AI lines attributed
// through a squash, we match the squash commit's landed lines against
// fingerprints of the AI lines we saw on the branch.
//
// A fingerprint is a one-way hash of a line's CONTENT. It is captured on-device
// whenever the attribution watcher sees a likely_ai line (on any branch), stored
// locally keyed by (rootKey, path), and NEVER emitted — the transfer runs
// entirely on the machine; only the resulting integer ranges + opaque lineageId
// leave. When the squash lands on default, durability seeding matches the new
// lines by fingerprint and transfers likely_ai onto the contiguous matched runs.
//
// Two guards keep a transfer from LYING about authorship (the one failure that
// matters most here):
//   - FILE-SCOPED: fingerprints are keyed by path, so identical boilerplate
//     (`}`) in a different file can never match.
//   - MIN RUN: only a contiguous run of >= durabilityMinTransferRun matched lines
//     transfers. A lone line that happens to equal an AI line (a stray `}`, a
//     re-typed import) is dropped to unknown rather than mis-attributed.

// durabilityMinTransferRun is the shortest contiguous run of fingerprint-matched
// lines that may transfer. 2 keeps a single coincidental line-match from
// attributing human code as AI, at the cost of missing genuine 1-line AI edits
// (a conservative undercount, never inflation). A var so tests can pin it.
var durabilityMinTransferRun = 2

// durabilityFingerprintTTLms bounds how long a captured AI fingerprint stays
// eligible to transfer — long enough for a normal PR to be reviewed and merged,
// short enough that stale fingerprints cannot resurface as false matches.
var durabilityFingerprintTTLms int64 = 14 * 24 * 60 * 60 * 1000

const durabilityFingerprintsVersion = 1

// durAiFingerprint is one AI line's content hash, the lineage it belongs to, and
// when it was captured (for TTL pruning). Hash only — never the line's bytes.
type durAiFingerprint struct {
	FP        string `json:"fp"`
	LineageID string `json:"lineageId"`
	BornTsMs  int64  `json:"bornTsMs"`
}

// durabilityFingerprints is the on-disk local store: rootKey → path → entries.
type durabilityFingerprints struct {
	V     int                                      `json:"v"`
	Roots map[string]map[string][]durAiFingerprint `json:"roots"`
}

func durabilityFingerprintsPath() string {
	return filepath.Join(state.StateDir(), "durability-fingerprints.json")
}

func loadDurabilityFingerprints() durabilityFingerprints {
	fps := durabilityFingerprints{V: durabilityFingerprintsVersion, Roots: map[string]map[string][]durAiFingerprint{}}
	_ = sign.WithBufferLock(durabilityFingerprintsPath()+".lock", func() error {
		data, err := os.ReadFile(durabilityFingerprintsPath())
		if err != nil {
			return nil
		}
		var onDisk durabilityFingerprints
		if json.Unmarshal(data, &onDisk) == nil && onDisk.V == durabilityFingerprintsVersion && onDisk.Roots != nil {
			fps = onDisk
		}
		return nil
	})
	return fps
}

func saveDurabilityFingerprints(fps durabilityFingerprints) {
	fps.V = durabilityFingerprintsVersion
	_ = sign.WithBufferLock(durabilityFingerprintsPath()+".lock", func() error {
		data, err := json.Marshal(fps)
		if err != nil {
			return err
		}
		tmp := durabilityFingerprintsPath() + ".tmp"
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return err
		}
		return os.Rename(tmp, durabilityFingerprintsPath())
	})
}

// lineFingerprint is the one-way hash used for a line's content. sha256 (already
// the codebase's identity primitive), truncated — collision-resistant enough
// that a false line-match is astronomically unlikely, and the file-scope +
// min-run guards catch the rare case anyway.
func lineFingerprint(content string) string {
	return ingest.Sha256Hex(content)[:16]
}

// parseUnifiedDiffNewLines maps, per changed file, each NEW-side line number to
// its content fingerprint. It reads ONLY `+` body lines (content hashed
// immediately, never retained) — mirroring parseUnifiedDiffHunks' header
// handling. At --unified=0 there is no context, so within a hunk each `+` line
// advances the new-side counter from the hunk's NewStart; `-` lines are old-side
// and ignored.
func parseUnifiedDiffNewLines(diff string) map[string]map[int]string {
	out := map[string]map[int]string{}
	oldPath, newPath := "", ""
	inBody := false
	newLine := 0
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			oldPath, newPath, inBody = "", "", false
		case !inBody && strings.HasPrefix(line, "--- "):
			oldPath = parseDiffOldPath(line[len("--- "):])
		case !inBody && strings.HasPrefix(line, "+++ "):
			newPath = parseDiffNewPath(line[len("+++ "):])
		case strings.HasPrefix(line, "@@ "):
			inBody = true
			m := durHunkRe.FindStringSubmatch(line)
			if m != nil {
				newLine = atoiDef(m[3], 0)
			}
		case inBody && strings.HasPrefix(line, "+"):
			path := newPath
			if path == "" {
				path = oldPath
			}
			if path == "" || newLine == 0 {
				continue
			}
			if out[path] == nil {
				out[path] = map[int]string{}
			}
			out[path][newLine] = lineFingerprint(line[1:])
			newLine++
			// `-` (old-side) lines match no case above and are skipped: they do
			// not advance the new-side counter.
		}
	}
	return out
}

// recordAiFingerprints captures, from ONE commit's diff, the content
// fingerprints of every likely_ai line, keyed by (rootKey, path). Reuses the
// caller's already-fetched diff (no extra spawn). Dedupes by fingerprint per
// path (identical lines collapse) and prunes TTL-expired entries. Best-effort:
// a nil/empty input simply records nothing.
func recordAiFingerprints(rootKey, sha, diff string, files []attrFile, nowMs int64) {
	if rootKey == "" || len(files) == 0 {
		return
	}
	newLines := parseUnifiedDiffNewLines(diff)
	captured := map[string][]durAiFingerprint{} // path -> new entries
	for _, f := range files {
		lines := newLines[f.Path]
		if len(lines) == 0 {
			continue
		}
		lineage := durLineageID(sha, f.Path)
		for _, r := range f.LineRanges {
			if r.Attribution != attributionLikelyAI {
				continue
			}
			for ln := r.Start; ln <= r.End; ln++ {
				if fp, ok := lines[ln]; ok {
					captured[f.Path] = append(captured[f.Path], durAiFingerprint{FP: fp, LineageID: lineage, BornTsMs: nowMs})
				}
			}
		}
	}
	if len(captured) == 0 {
		return
	}

	store := loadDurabilityFingerprints()
	if store.Roots == nil {
		store.Roots = map[string]map[string][]durAiFingerprint{}
	}
	paths := store.Roots[rootKey]
	if paths == nil {
		paths = map[string][]durAiFingerprint{}
	}
	for path, entries := range captured {
		seen := map[string]bool{}
		var kept []durAiFingerprint
		// Existing (TTL-pruned) first so the freshest bornTs of a duplicate wins
		// on the append below (dedup keeps the first seen; existing are older).
		for _, e := range append(append([]durAiFingerprint{}, paths[path]...), entries...) {
			if nowMs-e.BornTsMs >= durabilityFingerprintTTLms {
				continue
			}
			if seen[e.FP] {
				continue
			}
			seen[e.FP] = true
			kept = append(kept, e)
		}
		paths[path] = kept
	}
	store.Roots[rootKey] = paths
	saveDurabilityFingerprints(store)
}

// fingerprintsForPath returns the live (TTL-pruned) fingerprint→lineageId map for
// a path, or nil when none. nil means "no fingerprint evidence" — the caller then
// falls back to path-level seeding.
func fingerprintsForPath(rootKey, path string, nowMs int64) map[string]string {
	store := loadDurabilityFingerprints()
	entries := store.Roots[rootKey][path]
	if len(entries) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, e := range entries {
		if nowMs-e.BornTsMs >= durabilityFingerprintTTLms {
			continue
		}
		if _, ok := out[e.FP]; !ok {
			out[e.FP] = e.LineageID
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// matchedAiRuns finds the new-side lines whose fingerprint matches a captured AI
// fingerprint and groups them into contiguous runs. Only runs of at least minRun
// lines transfer (the false-positive guard); shorter runs are dropped to unknown.
// Each returned span carries the lineageId of its first matched line.
func matchedAiRuns(newLines map[int]string, fps map[string]string, minRun int) []durTrackedRange {
	if len(newLines) == 0 || len(fps) == 0 {
		return nil
	}
	nums := make([]int, 0, len(newLines))
	for ln := range newLines {
		nums = append(nums, ln)
	}
	sort.Ints(nums)

	var runs []durTrackedRange
	var cur *durTrackedRange
	flush := func() {
		if cur != nil {
			if cur.End-cur.Start+1 >= minRun {
				runs = append(runs, *cur)
			}
			cur = nil
		}
	}
	for i, ln := range nums {
		lineage, matched := fps[newLines[ln]]
		if !matched {
			flush()
			continue
		}
		if cur != nil && i > 0 && ln == nums[i-1]+1 {
			cur.End = ln
			continue
		}
		flush()
		cur = &durTrackedRange{Start: ln, End: ln, LineageID: lineage}
	}
	flush()
	return runs
}
