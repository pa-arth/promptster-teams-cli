package capture

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/outbox"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
)

// LineOrigin is a versioned, content-free SZZ receipt. Source and paths stay local.
// Unknown attribution is intentionally not a human label.
type LineOrigin struct {
	Version       int           `json:"version"`
	CommitSha     string        `json:"commitSha"`
	WorkspaceKey  string        `json:"workspaceKey"`
	State         string        `json:"state"`
	ReplacedLines int           `json:"replacedLines"`
	TracedLines   int           `json:"tracedLines"`
	SkippedFiles  int           `json:"skippedFiles"`
	CappedFiles   int           `json:"cappedFiles"`
	ReplacedFrom  []OriginCount `json:"replacedFrom"`
}
type OriginCount struct {
	Sha   string `json:"sha"`
	Lines int    `json:"lines"`
}
type oldSpan struct{ start, count int }

var originHunk = regexp.MustCompile(`(?m)^@@ -(\d+)(?:,(\d+))? \+\d+(?:,\d+)? @@`)
var originSHA = regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)
var originBlameHeader = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64}) \d+ (\d+) (\d+)$`)

// Both subprocess time and stdout are bounded. No stderr or source reaches logs.
type originOutput struct{ bytes.Buffer }

func (b *originOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 8*1024*1024 {
		return 0, errors.New("origin output limit")
	}
	return b.Buffer.Write(p)
}
func originGit(ctx context.Context, root string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-pager", "--literal-pathspecs", "-C", root}, args...)...)
	var out originOutput
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, errors.New("local git unavailable")
	}
	return out.Bytes(), nil
}

// AnalyzeLineOrigin blames old-side changed/deleted ranges at the parent, never
// today's working tree. Rename pairs come from NUL-delimited raw metadata;
// deleted files still exist at the parent. Merge commits compare with the first parent, matching the net PR fix. Shallow boundaries remain unknown.
func AnalyzeLineOrigin(root, sha string) (LineOrigin, error) {
	r := LineOrigin{Version: 1, CommitSha: sha, WorkspaceKey: workspaceKey(root), State: "measured", ReplacedFrom: []OriginCount{}}
	if !originSHA.MatchString(sha) {
		return r, errors.New("expected full commit SHA")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	object, err := originGit(ctx, root, "cat-file", "-p", sha)
	if err != nil {
		return r, err
	}
	var parents []string
	for _, l := range strings.Split(string(object), "\n") {
		if l == "" {
			break
		}
		if strings.HasPrefix(l, "parent ") {
			parents = append(parents, strings.TrimPrefix(l, "parent "))
		}
	}
	if len(parents) == 0 {
		return r, nil
	}
	parent := parents[0]
	raw, err := originGit(ctx, root, "diff", "--raw", "-z", "--no-ext-diff", "--no-textconv", "-M", parent, sha)
	if err != nil {
		r.State = "unavailable"
		r.SkippedFiles = 1
		return r, nil
	}
	parts := strings.Split(string(raw), "\x00")
	counts := map[string]int{}
	files := 0
	for i := 0; i+1 < len(parts); {
		header := strings.Fields(parts[i])
		i++
		if len(header) != 5 {
			return r, errors.New("invalid git raw metadata")
		}
		oldPath := parts[i]
		newPath := oldPath
		i++
		status := header[4]
		if strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C") {
			if i >= len(parts) {
				return r, errors.New("invalid rename metadata")
			}
			newPath = parts[i]
			i++
		}
		if status == "A" || strings.HasPrefix(status, "C") {
			continue
		}
		files++
		if files > 20 {
			r.CappedFiles++
			continue
		}
		// Compare the exact blob pair. Pathspec unions can include a second rename
		// in a path swap, duplicating its hunks under the wrong old filename.
		args := []string{"diff", "--no-ext-diff", "--no-textconv", "--unified=0"}
		if status == "D" {
			args = append(args, parent, sha, "--", oldPath)
		} else {
			args = append(args, parent+":"+oldPath, sha+":"+newPath)
		}
		diff, err := originGit(ctx, root, args...)
		if err != nil {
			r.SkippedFiles++
			continue
		}
		var spans []oldSpan
		for _, m := range originHunk.FindAllStringSubmatch(string(diff), -1) {
			start, _ := strconv.Atoi(m[1])
			n := 1
			if m[2] != "" {
				n, _ = strconv.Atoi(m[2])
			}
			if n > 0 {
				spans = append(spans, oldSpan{start, n})
				r.ReplacedLines += n
			}
		}
		if len(spans) == 0 {
			// Binary changes have no hunks, but are not an observed zero.
			if bytes.Contains(diff, []byte("Binary files ")) || bytes.Contains(diff, []byte("GIT binary patch")) {
				r.SkippedFiles++
			}
			continue
		}
		blame, err := originGit(ctx, root, "blame", "--incremental", parent, "--", oldPath)
		if err != nil {
			r.SkippedFiles++
			continue
		}
		boundary := map[string]bool{}
		// Incremental records mark shallow/root boundary ownership. A root commit is
		// valid ownership only in a complete checkout; shallow history is conservative.
		shallow, _ := originGit(ctx, root, "rev-parse", "--is-shallow-repository")
		current := ""
		for _, line := range strings.Split(string(blame), "\n") {
			if m := originBlameHeader.FindStringSubmatch(line); m != nil {
				current = m[1]
			}
			if line == "boundary" && strings.TrimSpace(string(shallow)) == "true" {
				boundary[current] = true
			}
		}
		covered := 0
		for _, line := range strings.Split(string(blame), "\n") {
			m := originBlameHeader.FindStringSubmatch(line)
			if m == nil || boundary[m[1]] {
				continue
			}
			start, _ := strconv.Atoi(m[2])
			n, _ := strconv.Atoi(m[3])
			for _, s := range spans {
				overlap := min(start+n, s.start+s.count) - max(start, s.start)
				if overlap > 0 {
					counts[m[1]] += overlap
					covered += overlap
				}
			}
		}
		expected := 0
		for _, s := range spans {
			expected += s.count
		}
		if covered != expected {
			r.SkippedFiles++
		}
		r.TracedLines += covered
	}
	for sha, n := range counts {
		r.ReplacedFrom = append(r.ReplacedFrom, OriginCount{sha, n})
	}
	sort.Slice(r.ReplacedFrom, func(i, j int) bool { return r.ReplacedFrom[i].Sha < r.ReplacedFrom[j].Sha })
	// Keep the wire payload below ingest limits even for highly fragmented history.
	if len(r.ReplacedFrom) > 1000 {
		r.ReplacedFrom = r.ReplacedFrom[:1000]
		r.TracedLines = 0
		for _, i := range r.ReplacedFrom {
			r.TracedLines += i.Lines
		}
		r.CappedFiles++
	}
	if r.SkippedFiles > 0 || r.CappedFiles > 0 {
		r.State = "partial"
	}
	if r.TracedLines == 0 && r.State == "partial" {
		r.State = "unavailable"
	}
	return r, nil
}
func lineOriginEvent(session Session, r LineOrigin) event.Event {
	e := event.NewEvent("commit_line_origin", unattributedSessionID(session.DeviceID))
	e.Source = presenceSource
	e.DeviceID = session.DeviceID
	e.Actor = event.SystemActor()
	e.Data = eventDataMap(r)
	return e
}

// BackfillLineOrigins emits history only; it never reconstructs historical AI
// attribution from today's path ledger. Existing capture supplies that join.
func BackfillLineOrigins(session Session, root string, limit int, dryRun bool) ([]LineOrigin, error) {
	if limit < 1 || limit > 500 {
		return nil, errors.New("limit must be 1..500")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	raw, err := originGit(ctx, root, "rev-list", "--first-parent", fmt.Sprintf("--max-count=%d", limit), "HEAD")
	if err != nil {
		return nil, err
	}
	var receipts []LineOrigin
	for _, sha := range strings.Fields(string(raw)) {
		r, err := AnalyzeLineOrigin(root, sha)
		if err != nil {
			return receipts, err
		}
		receipts = append(receipts, r)
		if !dryRun {
			e := lineOriginEvent(session, r)
			if err := sign.AppendEventToLocalBuffer(&e, false); err != nil {
				return receipts, errors.New("origin signing failed")
			}
			if err := outbox.AppendTo(outbox.LaneBackfill(), e); err != nil {
				return receipts, errors.New("origin queue failed")
			}
		}
	}
	return receipts, nil
}

// RunLineOriginBackfill is an explicit, bounded historical replay. Dry-run works
// without credentials and prints only metadata. Queueing uses the replay lane;
// the existing daemon owns delivery and authentication.
func RunLineOriginBackfill(root string, limit int, dryRun bool) ([]LineOrigin, error) {
	session := Session{}
	if !dryRun {
		var err error
		session, err = loadSession()
		if err != nil {
			return nil, err
		}
	}
	return BackfillLineOrigins(session, root, limit, dryRun)
}
