package capture

import (
	"path"
	"strings"
	"sync"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
	tiktoken "github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
)

// On-device token counting of AI-authored committed lines.
//
// commit_attribution carries aiTokens: the o200k tiktoken count of a commit's
// likely_ai ADDED lines, computed ON-DEVICE from the diff bytes already in hand
// (no new git spawn) at attribution time. It is the denominator source for the
// backend's token-efficiency ratio. Only the SCALAR count leaves — never the
// line text, bytes, or fingerprints (source-free, like every other field on the
// event).
//
// The encoder is o200k_base (matching the backend's js-tiktoken o200k so the two
// counts are consistent) and is loaded from the EMBEDDED/offline BPE ranks —
// tiktoken_loader.NewOfflineLoader reads the ranks baked into the binary, so
// there is NO runtime network fetch. It is initialized exactly once (sync.Once)
// and reused for every commit and line; the BPE load is expensive and must not
// repeat per commit or per line.

var (
	tiktokenOnce sync.Once
	tiktokenEnc  *tiktoken.Tiktoken
)

// tiktokenO200kEncoder lazily builds the shared o200k encoder from the offline
// (embedded) BPE loader, exactly once. Returns nil if init fails — callers then
// count 0 rather than block or fetch.
func tiktokenO200kEncoder() *tiktoken.Tiktoken {
	tiktokenOnce.Do(func() {
		// Offline/embedded BPE ranks — NEVER a runtime network fetch. This makes
		// the CLI work offline and keeps init constant-time.
		tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
		enc, err := tiktoken.GetEncoding(tiktoken.MODEL_O200K_BASE)
		if err != nil {
			state.HookDebugf("tiktoken o200k init failed (aiTokens will be 0): %v", err)
			return
		}
		tiktokenEnc = enc
	})
	return tiktokenEnc
}

// countTiktokenTokens returns the o200k token count of text, using the cached
// encoder. Empty text (and a failed encoder init) is 0 tokens.
func countTiktokenTokens(text string) int {
	if text == "" {
		return 0
	}
	enc := tiktokenO200kEncoder()
	if enc == nil {
		return 0
	}
	return len(enc.Encode(text, nil, nil))
}

// commitAiTokens returns the o200k tiktoken count of a commit's likely_ai ADDED
// lines. It walks the SAME likely_ai line numbers recordAiFingerprints does —
// reusing mapUnifiedDiffNewLines (identity transform) to recover each added
// line's text from the diff already in hand — joins them with newlines, and
// encodes ONCE. A commit with no likely_ai added line → 0.
//
// SOURCE-FREE: the joined text is tokenized and discarded here; only the scalar
// count is returned. No new git spawn (the diff is the caller's).
func commitAiTokens(diff string, files []attrFile) int {
	if diff == "" || len(files) == 0 {
		return 0
	}
	newLines := mapUnifiedDiffNewLines(diff, func(s string) string { return s })
	var b strings.Builder
	for _, f := range files {
		lines := newLines[f.Path]
		if len(lines) == 0 || notAuthoredCode(f) {
			continue
		}
		var fb strings.Builder
		n := 0
		for _, r := range f.LineRanges {
			if r.Attribution != attributionLikelyAI {
				continue
			}
			for ln := r.Start; ln <= r.End; ln++ {
				if txt, ok := lines[ln]; ok {
					fb.WriteString(txt)
					fb.WriteByte('\n')
					n++
				}
			}
		}
		// Lines AND bytes: a compact data file can be one enormous line.
		if n > maxAuthoredLinesPerFile || fb.Len() > maxAuthoredBytesPerFile {
			continue
		}
		b.WriteString(fb.String())
	}
	if b.Len() == 0 {
		return 0
	}
	return countTiktokenTokens(strings.TrimSuffix(b.String(), "\n"))
}

// maxAuthoredLinesPerFile / maxAuthoredBytesPerFile cap how much likely_ai
// content one file may add to a commit and still count as code the model wrote.
// Over either, it stays out of aiTokens; over the line cap it also stays out of
// the fingerprint store, where one generated file would otherwise evict every
// other file's entries from the bounded store.
// ponytail: size caps, not a classifier — a genuine >5k-line agent-written file
// is undercounted and loses squash-merge evidence; replace with a generated/data
// detector if that ever shows up.
const maxAuthoredLinesPerFile = 5000
const maxAuthoredBytesPerFile = 200_000

// notAuthoredCodeExts are files whose lines are data or tool output, never code
// the model emitted token by token.
var notAuthoredCodeExts = map[string]bool{
	".jsonl": true, ".ndjson": true, ".csv": true, ".tsv": true,
	".lock": true, ".sum": true, ".snap": true, ".map": true,
}

// notAuthoredCode reports whether a file's likely_ai lines must stay out of
// aiTokens and the fingerprint store by KIND: a lockfile an install rewrote, a
// data dump. Size is judged separately (maxAuthoredBytesPerFile). Path-level attribution marks every added line of
// an AI-touched file likely_ai, so one such file swamps the count — on
// 2026-10-04 three commits (a 757k-line tokenizer.json, scraped .jsonl) were
// 53.2M of an org's 54.2M merged tokens and put generated:merged below 1:1.
// The attribution event itself is unchanged; only the token count skips them.
func notAuthoredCode(f attrFile) bool {
	if f.GenerationKind == "dependency" {
		return true
	}
	base := path.Base(f.Path)
	switch base {
	case "package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml":
		return true
	}
	return notAuthoredCodeExts[strings.ToLower(path.Ext(base))] || strings.HasSuffix(base, ".min.js")
}
