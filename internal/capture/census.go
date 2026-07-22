package capture

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
	"github.com/pa-arth/promptster-teams-cli/internal/outbox"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// Config census.
//
// Once per `watch` startup — and every 24h while running — the CLI takes an
// inventory of the engineer's local agent configuration and emits ONE
// `config_census` event. The backend uses it to price the "config tax": how
// many tokens of always-loaded context (CLAUDE.md files, skill listings,
// plugin listings, MCP tool schemas) every single request pays for, and
// whether that inventory is actually earning its keep (skill/MCP ROI).
//
// HARD PRIVACY RULE: the census carries token COUNTS (chars/4) and NAMES only
// — never file contents. CLAUDE.md bodies, skill bodies, plugin sources, and
// MCP schemas never leave the machine; the only content-derived values are
// integer size estimates. TestConfigCensusCarriesNoFileContents pins this.

const (
	// configCensusInterval is how often a long-running watch re-takes the
	// census.
	configCensusInterval = 24 * time.Hour
	// configCensusCheckInterval is how often the ticker CHECKS whether the
	// interval has elapsed. Checking hourly (against a persisted cursor)
	// instead of a raw 24h ticker keeps the cadence right through laptop
	// sleeps and watch restarts.
	configCensusCheckInterval = time.Hour
)

// censusSkill is one entry of ~/.claude/skills — identity + listing cost only.
type censusSkill struct {
	Slug string `json:"slug"` // directory name
	Name string `json:"name"` // frontmatter `name:`, else slug
	// DescTokens estimates (chars/4) what the skill's `description:` costs in
	// the always-loaded skill listing.
	DescTokens int `json:"descTokens"`
}

// censusPlugin is one enabled plugin — name + listing cost only.
type censusPlugin struct {
	Name string `json:"name"`
	// ListingTokens estimates (chars/4) the plugin's contribution to
	// always-loaded context: the names + descriptions of the skills, commands,
	// and agents it ships. 0 when the install layout isn't enumerable.
	ListingTokens int `json:"listingTokens"`
}

// censusMCPServer is one configured MCP server — name + load mode only.
type censusMCPServer struct {
	Name string `json:"name"`
	// Deferred is true when the server's tools load lazily (tool-search /
	// deferred loading). No config-level indicator exists today, so this is
	// currently always false; the field is kept so the shape doesn't change
	// when detection lands.
	Deferred bool `json:"deferred"`
}

// configCensusData is the CLOSED payload of a config_census event. Counts and
// names only — adding any field that carries file contents is forbidden.
type configCensusData struct {
	// WorkspaceKey is a stable, privacy-safe identity for the active workspace
	// so the backend can de-dupe workspaces across sessions (e.g. "share of my
	// repos with a healthy CLAUDE.md"). It is the git remote slug (owner/name)
	// when the workspace is a git repo with an `origin` remote — matching the
	// outcome_events.repo convention so coverage can correlate with PR outcomes
	// — else an opaque sha256(abspath) hash. It NEVER carries a filesystem path
	// or file contents. Empty only when there is no workspace.
	WorkspaceKey         string `json:"workspaceKey"`
	GlobalClaudeMdTokens int    `json:"globalClaudeMdTokens"`
	// ProjectClaudeMdTokens is the token cost; ProjectClaudeMdPosition says WHERE
	// it came from, which decides whether those tokens are actually loaded.
	//
	// Magnitude alone cannot answer both questions asked of this field. Claude
	// Code loads CLAUDE.md at or above the working directory AT LAUNCH; a file in
	// a sub-package loads only when the agent first reads something in that
	// directory, and is NOT re-injected after /compact. So for a nested file,
	// "does this repo have project memory?" is YES while "what standing per-turn
	// cost does it carry?" is ~NOTHING — opposite answers, one integer.
	//
	// "root"   — a watched root carries CLAUDE.md: always-loaded, bill it.
	// "nested" — only a sub-package does: real memory, LATENT. Counts as covered,
	//            must not be priced as standing context.
	// "absent" — no project memory tokens found at all.
	//
	// omitempty: an older CLI omits it, and the backend must read that as
	// "position unknown", never as the "absent" value.
	ProjectClaudeMdTokens   int               `json:"projectClaudeMdTokens"`
	ProjectClaudeMdPosition string            `json:"projectClaudeMdPosition,omitempty"`
	Skills                  []censusSkill     `json:"skills"`
	SkillListingTokens      int               `json:"skillListingTokens"`
	SkillCount              int               `json:"skillCount"`
	Plugins                 []censusPlugin    `json:"plugins"`
	PluginListingTokens     int               `json:"pluginListingTokens"`
	PluginCount             int               `json:"pluginCount"`
	MCPServers              []censusMCPServer `json:"mcpServers"`
	MCPDeferred             bool              `json:"mcpDeferred"`
	// ClaudeTranscriptsTotal / ClaudeTranscriptsActive7d are content-free
	// capture-health counts: how many Claude Code transcript JSONL files exist
	// under ~/.claude/projects, and how many were modified in the last 7 days.
	// They let the backend tell a BROKEN-capture engineer (transcripts on disk
	// but nothing ingested) apart from a not-using-Claude-locally engineer (no
	// transcripts at all). Two integers only — never a path, filename, or repo
	// slug, so the "never store your code" guarantee holds (a dir/slug name is
	// source metadata). Stat-only: file contents are never opened.
	//
	// Pointers + omitempty: nil (omitted) means "could not read the projects
	// tree" — the counts are unreliable, so the backend must read UNKNOWN, not a
	// false zero that would look like "not using Claude Code". A present 0 is a
	// definite zero (dir absent = genuinely no local transcripts).
	ClaudeTranscriptsTotal    *int `json:"claudeTranscriptsTotal,omitempty"`
	ClaudeTranscriptsActive7d *int `json:"claudeTranscriptsActive7d,omitempty"`
}

// censusEnv points the census builder at the config surfaces it inventories.
// Parametrized (rather than reading globals) so tests can build a census from
// a fixture directory.
type censusEnv struct {
	claudeDir      string   // Claude Code config root (claudeConfigDir())
	claudeJSONPath string   // ~/.claude.json (global MCP server registry)
	workspaceRoots []string // the same roots the transcript watcher matches
}

func defaultCensusEnv(workspace string) censusEnv {
	home, _ := os.UserHomeDir()
	roots := []string{}
	if workspace != "" {
		roots = workspaceMatchRoots(resolvePath(workspace))
	}
	return censusEnv{
		claudeDir:      claudeConfigDir(),
		claudeJSONPath: filepath.Join(home, ".claude.json"),
		workspaceRoots: roots,
	}
}

// approxTokens is the census's token estimator: chars/4. Deliberately crude —
// the census compares configs against each other, not against a tokenizer.
func approxTokens(chars int) int { return chars / 4 }

// fileTokens estimates a file's token cost from its SIZE — the content is
// never read into the payload path.
func fileTokens(path string) int {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return 0
	}
	return approxTokens(int(info.Size()))
}

// claudeMdMaxDepth bounds how many directory levels below a workspace root the
// nested-CLAUDE.md fallback scan descends. Claude Code discovers CLAUDE.md
// HIERARCHICALLY (a repo may keep its memory in a sub-package, e.g.
// my-clerk-next-app/CLAUDE.md), so the census must too — but an unbounded walk
// on a large monorepo is a cost this once-per-24h census should not pay.
// Sub-package CLAUDE.md files sit within a few levels; 5 covers the real layouts
// (apps/api/CLAUDE.md, packages/x/CLAUDE.md) with margin. A file up to 5 levels
// below the root is included; deeper is skipped.
const claudeMdMaxDepth = 5

// projectClaudeMdPosition values. Mirrored by a zod enum in the backend contract
// (packages/contracts/src/configCensus.ts) — changing a string here without
// changing it there strips the field at ingest validation, silently.
const (
	claudeMdPositionRoot   = "root"
	claudeMdPositionNested = "nested"
	claudeMdPositionAbsent = "absent"
)

// projectClaudeMdSkipDirs are directory names the nested scan never descends
// into: a CLAUDE.md inside a dependency/build/vendor tree is either a third
// party's or a build-time COPY of the repo's own memory, not authored project
// memory — and counting it would overstate the fallback below. Hidden
// directories (".*") are skipped separately by the walker — that also excludes
// .git and, crucially, .claude/worktrees, whose nested repo copies would
// otherwise be picked up.
var projectClaudeMdSkipDirs = map[string]bool{
	"node_modules": true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
	"out":          true,
	"target":       true,
	"__pycache__":  true,
}

// projectClaudeMdTokens estimates the tokens of project CLAUDE.md Claude Code
// loads for work in the given workspace roots.
//
// It reports the ALWAYS-LOADED memory first: the CLAUDE.md at a workspace root,
// which every request in that repo pays for (summed across the watched roots, as
// the config-tax framing intends). ONLY when no root carries one does it fall
// back to the largest CLAUDE.md nested in a sub-package — the monorepo shape
// (e.g. my-clerk-next-app/CLAUDE.md) that otherwise reported 0 and scored the
// whole workspace 0% on the cc-audit coverage check.
//
// The fallback takes the MAX single nested file, never a sum: sibling packages'
// memories don't co-load on one request (a request under packages/a never loads
// packages/b/CLAUDE.md), so summing them would overstate always-loaded context.
// Preferring the root also means every repo that already had a root CLAUDE.md
// reports exactly what it did before — this change only lifts the false 0 for
// repos whose memory lives in a sub-directory. Content is never read —
// fileTokens stats size only, preserving the no-file-contents guarantee.
//
// The second return is WHICH branch supplied the count. The fallback made this
// function answer two questions with one number, and for a nested file the right
// answers are opposite: covered (yes, memory exists) but not always-loaded (no,
// it isn't standing context). Reporting position lets each consumer read the one
// it actually asked about instead of both reading the same integer wrong.
func projectClaudeMdTokens(roots []string) (int, string) {
	seenRoots := map[string]bool{}
	rootTokens := 0
	for _, root := range roots {
		if root == "" || seenRoots[root] {
			continue
		}
		seenRoots[root] = true
		rootTokens += fileTokens(filepath.Join(root, "CLAUDE.md"))
	}
	if rootTokens > 0 {
		return rootTokens, claudeMdPositionRoot
	}
	if nested := maxNestedClaudeMdTokens(roots); nested > 0 {
		return nested, claudeMdPositionNested
	}
	// Position describes where the reported TOKENS came from, not what exists on
	// disk: a root CLAUDE.md that is empty (or too small to round to a token)
	// yields 0 here and reports "absent". That is the honest reading for every
	// consumer — a zero-token file is zero always-loaded context and zero
	// coverage, so there is no question the distinction would answer.
	return 0, claudeMdPositionAbsent
}

// isGitRepoRoot reports whether root is the top of a git working tree — a
// primary checkout (.git is a directory) or a linked worktree (.git is a file
// with a gitdir: pointer). One stat per root: constant-time and off the 24h
// census path. Empty root → false.
func isGitRepoRoot(root string) bool {
	if root == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(root, ".git"))
	return err == nil
}

// maxNestedClaudeMdTokens returns the token cost of the LARGEST CLAUDE.md nested
// below any of the workspace roots, or 0 when none is found. Bounded by
// claudeMdMaxDepth and projectClaudeMdSkipDirs (+ hidden dirs); every branch is
// best-effort — an unreadable subtree is skipped, never surfaced as an error.
func maxNestedClaudeMdTokens(roots []string) int {
	best := 0
	seenRoots := map[string]bool{}
	for _, root := range roots {
		if root == "" || seenRoots[root] {
			continue
		}
		seenRoots[root] = true
		// Only ever descend into an actual git repository. The autostart daemon's
		// workspace falls back to the user's HOME dir (launchd
		// WorkingDirectory=home, no PROMPTSTER_TEAMS_WATCH_DIR set), and a 5-level
		// WalkDir of home enumerates ~/Documents, ~/Downloads, ~/Music — every
		// macOS TCC-protected folder — firing "wants to access your Downloads"
		// consent prompts from a capture tool. A non-repo root has no project
		// CLAUDE.md to find anyway, so gating the walk on .git presence both fixes
		// the prompt storm and matches the census's per-repo intent.
		if !isGitRepoRoot(root) {
			continue
		}
		rootDepth := strings.Count(filepath.Clean(root), string(os.PathSeparator))
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				// Unreadable entry: skip its subtree if a dir, else the file —
				// never abort the whole scan.
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				if path == root {
					return nil
				}
				name := d.Name()
				if strings.HasPrefix(name, ".") || projectClaudeMdSkipDirs[name] {
					return filepath.SkipDir
				}
				if strings.Count(filepath.Clean(path), string(os.PathSeparator))-rootDepth > claudeMdMaxDepth {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Name() != "CLAUDE.md" {
				return nil
			}
			if t := fileTokens(path); t > best {
				best = t
			}
			return nil
		})
	}
	return best
}

// primaryWorkspaceRoot returns the first non-empty workspace root — the
// engineer's active workspace. Worktrees of the same repo share an origin
// remote, so any of them yields the same WorkspaceKey; the first is canonical.
func primaryWorkspaceRoot(roots []string) string {
	for _, r := range roots {
		if r != "" {
			return r
		}
	}
	return ""
}

// workspaceKey derives a stable, privacy-safe identity for a workspace. It
// PREFERS the git remote slug (owner/name) so backend rollups can correlate
// CLAUDE.md coverage with PR outcomes (outcome_events.repo uses the same
// convention); when the workspace is not a git repo with an origin remote it
// falls back to an opaque sha256 hash truncated to 16 hex chars. The result
// never carries a filesystem path or file contents. Empty root → "".
//
// The hash fallback keys off the REPO ROOT (gitRepoRoot), not the raw path, so
// every subdir and linked worktree of one no-remote repo collapses to a single
// key — matching how the slug path already yields one identity from any subdir
// of a repo WITH an origin remote. Only a dir that is not inside any git repo at
// all falls all the way back to hashing its own abspath.
func workspaceKey(root string) string {
	if root == "" {
		return ""
	}
	if slug := gitRemoteSlug(root); slug != "" {
		return slug
	}
	return workspaceHashKey(root)
}

// workspaceHashKey is workspaceKey's opaque fallback, split out so a caller that
// has ALREADY resolved the remote (sessionRepoIdentity) can reach the fallback
// without paying for a second `git config` spawn. Never call it in place of
// workspaceKey — on a repo that HAS an origin remote it would discard the
// joinable slug and hash the root instead.
func workspaceHashKey(root string) string {
	target := gitRepoRoot(root)
	if target == "" {
		target = root
		if a, err := filepath.Abs(root); err == nil {
			target = a
		}
	}
	return ingest.Sha256Hex(target)[:16]
}

// gitRepoRoot canonicalizes dir to the root of its git repository — the SAME
// path for a repo's primary checkout and all of its linked worktrees, and for
// any subdirectory of either — so worktrees/subdirs of one repo collapse to a
// single identity. It resolves the shared git dir (`--git-common-dir`, which
// points at the one `.git` every worktree of a repo shares) and returns its
// parent. Returns "" when dir is not inside a git repo. Timeout-bounded like
// gitRemoteSlug so a network-mounted or corrupt .git can't stall the census.
func gitRepoRoot(dir string) string {
	if dir == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// #nosec G204 -- constant argv; dir is a discovered workspace/transcript cwd, not user input. Reads only the local git layout, timeout-bounded.
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return ""
	}
	common := strings.TrimSpace(string(out))
	if common == "" {
		return ""
	}
	// `--git-common-dir` is reported relative to dir for a primary checkout
	// (".git") and absolute for a linked worktree; normalize both to an absolute
	// path before taking the parent (the repo root).
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	return resolvePath(filepath.Dir(common))
}

// gitRemoteSlug returns the workspace's origin remote as an owner/name slug, or
// "" when the dir is not a git repo, has no origin remote, or the URL can't be
// reduced to owner/name. Only the slug leaves the machine — never the URL.
func gitRemoteSlug(root string) string {
	_, slug := gitRemote(root)
	return slug
}

// gitRemote returns the workspace's origin remote as a (host, owner/name) pair.
// Both are "" together when the dir is not a git repo, has no origin remote, or
// the URL can't be reduced to owner/name. Only these two reduced fields ever
// leave the machine — never the URL, and never a filesystem path.
func gitRemote(root string) (host, slug string) {
	// A local `git config` read is normally milliseconds, but on a
	// network-mounted workspace or a corrupt .git it can hang indefinitely —
	// bound it so census never stalls the watch process.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// #nosec G204 -- constant argv; root is a discovered workspace dir, not user input. Reads only the local origin URL, timeout-bounded.
	out, err := exec.CommandContext(ctx, "git", "-C", root, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return "", ""
	}
	return normalizeRemote(string(out))
}

// normalizeRemoteSlug reduces a git remote URL to its trailing owner/name.
// Thin wrapper over normalizeRemote — kept so existing callers and their tests
// are unchanged, and so the slug can never drift from the host it was parsed
// alongside.
func normalizeRemoteSlug(raw string) string {
	_, slug := normalizeRemote(raw)
	return slug
}

// normalizeRemote reduces a git remote URL to a (host, owner/name) pair,
// stripping scheme, userinfo, port, and a trailing ".git". Handles the common
// forms: https://host/owner/name(.git), ssh://git@host/owner/name(.git), and
// the scp-style git@host:owner/name(.git). Returns ("", "") when it can't
// isolate an owner and name. Taking only the last two path segments guarantees
// no full filesystem path can survive into the identity.
//
// WHY THE HOST IS CAPTURED SEPARATELY. The slug alone is ambiguous across
// providers: gitlab.com/acme/api and github.com/acme/api both reduce to the
// identical string "acme/api". The backend uses the slug to decide whether a
// repo belongs to the company's connected GitHub org — without a host, an owner
// name that merely collides reads as a match, which at a GitLab shop would
// misclassify repos for every engineer. The host is emitted so the backend can
// require a provider match instead of guessing, and abstain when it has none.
//
// The host is lowercased and port-stripped for stable comparison; userinfo
// ("git@") is discarded rather than reduced, since a username is personal data
// with no bearing on which provider hosts the repo. The two returns are always
// produced together or not at all: a URL the parser rejects yields ("", ""), so
// there is no state where a host survives from an identity we refused to form.
func normalizeRemote(raw string) (host, slug string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	raw = strings.TrimSuffix(raw, ".git")
	if i := strings.Index(raw, "://"); i >= 0 {
		// scheme://[user@]host/owner/name — drop scheme, keep host. A non-empty
		// host must sit between the "://" and the first "/"; an empty host (as in
		// file:///home/alice/repo) means a local path, not a hosted remote —
		// reject so no filesystem segment can survive into the identity.
		if strings.EqualFold(raw[:i], "file") {
			return "", ""
		}
		rest := raw[i+3:]
		j := strings.Index(rest, "/")
		if j <= 0 {
			return "", ""
		}
		host = normalizeRemoteHost(rest[:j])
		raw = rest[j+1:]
	} else if i := strings.Index(raw, ":"); i >= 0 {
		// scp-style [user@]host:owner/name — everything after the FIRST colon. The
		// scp form has no port syntax, so the authority cannot itself contain a
		// colon; the first one is always the separator. (Splitting on the LAST colon
		// instead would hand a path that contains one, `git@host:a:b/c`, a truncated
		// host and the wrong slug.) Reject forms that are actually filesystem paths:
		// a host with a slash, or a path after the colon (C:/Users/alice/repo,
		// git@host:/abs/path).
		rawHost, rest := raw[:i], raw[i+1:]
		if rawHost == "" || strings.ContainsAny(rawHost, "/\\") || strings.HasPrefix(rest, "/") {
			return "", ""
		}
		host = normalizeRemoteHost(rawHost)
		raw = rest
	} else {
		// No scheme and no colon → a bare local path (/home/alice/repo, ./repo)
		// or junk, never a hosted remote. Reject so the WorkspaceKey never
		// encodes a filesystem path (it falls back to the hashed root instead).
		return "", ""
	}
	parts := strings.Split(strings.Trim(raw, "/"), "/")
	if len(parts) < 2 {
		return "", ""
	}
	owner, name := parts[len(parts)-2], parts[len(parts)-1]
	if owner == "" || name == "" {
		return "", ""
	}
	return host, owner + "/" + name
}

// normalizeRemoteHost reduces a URL authority to a bare comparable hostname:
// userinfo and port dropped, lowercased. Returns "" if nothing is left, so a
// malformed authority degrades to "no host known" rather than to junk the
// backend would compare against a real provider host.
func normalizeRemoteHost(authority string) string {
	// Userinfo may itself contain "@" in a password, so split on the LAST one.
	if i := strings.LastIndex(authority, "@"); i >= 0 {
		authority = authority[i+1:]
	}
	// Strip a :port. An IPv6 literal is bracketed ("[::1]:22"), so only cut at a
	// colon that follows the closing bracket; a bare colon in an unbracketed host
	// is a port separator.
	if strings.HasPrefix(authority, "[") {
		if j := strings.Index(authority, "]"); j >= 0 {
			authority = authority[:j+1]
		}
	} else if i := strings.LastIndex(authority, ":"); i >= 0 {
		authority = authority[:i]
	}
	return strings.ToLower(strings.TrimSpace(authority))
}

// buildConfigCensus inventories the config surfaces under env. Every branch is
// best-effort: a missing file/dir contributes zeros, never an error.
func buildConfigCensus(env censusEnv) configCensusData {
	data := configCensusData{
		WorkspaceKey:         workspaceKey(primaryWorkspaceRoot(env.workspaceRoots)),
		GlobalClaudeMdTokens: fileTokens(filepath.Join(env.claudeDir, "CLAUDE.md")),
		Skills:               []censusSkill{},
		Plugins:              []censusPlugin{},
		MCPServers:           []censusMCPServer{},
	}

	// Project CLAUDE.md: the always-loaded root file, or the largest nested one
	// when the repo keeps its memory in a sub-package (see projectClaudeMdTokens).
	// Position rides along so the backend can tell those two apart — a nested file
	// is covered but latent, and pricing it as always-on overstates the config tax.
	data.ProjectClaudeMdTokens, data.ProjectClaudeMdPosition = projectClaudeMdTokens(env.workspaceRoots)

	data.Skills = censusSkills(filepath.Join(env.claudeDir, "skills"))
	for _, s := range data.Skills {
		data.SkillListingTokens += s.DescTokens
	}
	data.SkillCount = len(data.Skills)

	data.Plugins = censusPlugins(env.claudeDir)
	for _, p := range data.Plugins {
		data.PluginListingTokens += p.ListingTokens
	}
	data.PluginCount = len(data.Plugins)

	data.MCPServers = censusMCPServers(env.claudeJSONPath, env.workspaceRoots)
	// No config-level deferred-loading indicator is detectable today.
	data.MCPDeferred = false

	data.ClaudeTranscriptsTotal, data.ClaudeTranscriptsActive7d = countClaudeTranscripts()

	return data
}

// countClaudeTranscripts tallies Claude Code transcript JSONL files under
// ClaudeProjectsDir(): total, and how many were modified within the last 7
// days. It is deliberately content-free — it STATS files (never opens them)
// and returns only two integers, so no path, filename, or repo slug can leak.
// A missing projects dir yields (0, 0). Mirrors candidateClaudeTranscripts'
// walk shape (recursive; subagent sidechain files under <session>/subagents/
// are counted — they are real transcript files on disk).
func countClaudeTranscripts() (total *int, active7d *int) {
	dir := ClaudeProjectsDir()
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// Projects dir absent = genuinely no local Claude Code transcripts: a
			// DEFINITE zero (the backend reads it as "no local CC", not "unknown").
			z0, z1 := 0, 0
			return &z0, &z1
		}
		// Dir exists but can't be statted (e.g. permissions) → unreliable; omit.
		return nil, nil
	}
	if !info.IsDir() {
		return nil, nil
	}
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	var t, a int
	walkFailed := false
	_ = filepath.Walk(dir, func(path string, fi os.FileInfo, werr error) error {
		if werr != nil {
			// A subtree we cannot read → the tally would be partial/untrustworthy.
			// Flag it so we report UNKNOWN rather than a false low count.
			walkFailed = true
			return nil
		}
		if fi.IsDir() {
			return nil
		}
		if !strings.HasSuffix(filepath.Base(path), ".jsonl") {
			return nil
		}
		t++
		if fi.ModTime().After(cutoff) {
			a++
		}
		return nil
	})
	if walkFailed {
		return nil, nil
	}
	return &t, &a
}

// censusSkills enumerates <skillsDir>/*/SKILL.md: slug = dir name, name =
// frontmatter `name:` (else slug), descTokens = chars/4 of the frontmatter
// `description:` value. The skill BODY is never measured or sent — only the
// description contributes to the always-loaded listing.
func censusSkills(skillsDir string) []censusSkill {
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return []censusSkill{}
	}
	skills := []censusSkill{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		slug := entry.Name()
		fm := readFrontmatter(filepath.Join(skillsDir, slug, "SKILL.md"))
		if fm == nil {
			continue
		}
		name := fm["name"]
		if name == "" {
			name = slug
		}
		skills = append(skills, censusSkill{
			Slug:       slug,
			Name:       name,
			DescTokens: approxTokens(len(fm["description"])),
		})
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Slug < skills[j].Slug })
	return skills
}

// censusPlugins reads enabled plugins from <claudeDir>/settings.json
// (`enabledPlugins`: name -> bool), falling back to the installed-plugin
// registry when settings carry none, and estimates each plugin's listing cost
// from its install directory.
func censusPlugins(claudeDir string) []censusPlugin {
	installPaths := pluginInstallPaths(filepath.Join(claudeDir, "plugins", "installed_plugins.json"))

	names := enabledPluginNames(filepath.Join(claudeDir, "settings.json"))
	if names == nil {
		for name := range installPaths {
			names = append(names, name)
		}
	}

	plugins := []censusPlugin{}
	for _, name := range names {
		plugins = append(plugins, censusPlugin{
			Name:          name,
			ListingTokens: pluginListingTokens(installPaths[name]),
		})
	}
	sort.Slice(plugins, func(i, j int) bool { return plugins[i].Name < plugins[j].Name })
	return plugins
}

// enabledPluginNames parses settings.json's `enabledPlugins` map, keeping only
// entries whose value is true. Returns nil when settings.json is absent,
// unparseable, or lacks the enabledPlugins key (no authoritative list);
// returns a non-nil (possibly empty) slice when the key is present, so
// callers can distinguish "everything explicitly disabled" from "no list".
func enabledPluginNames(settingsPath string) []string {
	// #nosec G304 -- settingsPath is the Claude settings.json under the config dir, not user input; only enabledPlugins keys are read.
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		return nil
	}
	var settings struct {
		EnabledPlugins map[string]bool `json:"enabledPlugins"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return nil
	}
	if settings.EnabledPlugins == nil {
		return nil
	}
	names := []string{}
	for name, enabled := range settings.EnabledPlugins {
		if enabled {
			names = append(names, name)
		}
	}
	return names
}

// pluginInstallPaths parses installed_plugins.json (v2 shape:
// {"plugins": {"name@marketplace": [{"installPath": ...}]}}) into
// name -> install path. Defensive: any parse miss yields an empty map.
func pluginInstallPaths(registryPath string) map[string]string {
	paths := map[string]string{}
	// #nosec G304 -- registryPath is the Claude installed_plugins.json under the config dir, not user input; only names + install paths are read.
	raw, err := os.ReadFile(registryPath)
	if err != nil {
		return paths
	}
	var registry struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &registry); err != nil {
		return paths
	}
	for name, installs := range registry.Plugins {
		for _, inst := range installs {
			if inst.InstallPath != "" {
				paths[name] = inst.InstallPath
				break
			}
		}
	}
	return paths
}

// pluginListingTokens estimates (chars/4) the names + descriptions a plugin
// contributes to always-loaded context: its skills' SKILL.md frontmatter and
// its commands/agents markdown frontmatter. 0 when the install dir is unknown
// or empty — never an error.
func pluginListingTokens(installPath string) int {
	if installPath == "" {
		return 0
	}
	tokens := 0
	// skills/*/SKILL.md — same layout as user skills.
	if entries, err := os.ReadDir(filepath.Join(installPath, "skills")); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			fm := readFrontmatter(filepath.Join(installPath, "skills", entry.Name(), "SKILL.md"))
			if fm == nil {
				continue
			}
			tokens += approxTokens(len(entry.Name()) + len(fm["description"]))
		}
	}
	// commands/*.md and agents/*.md — name (filename) + frontmatter description.
	for _, sub := range []string{"commands", "agents"} {
		entries, err := os.ReadDir(filepath.Join(installPath, sub))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			fm := readFrontmatter(filepath.Join(installPath, sub, entry.Name()))
			desc := ""
			if fm != nil {
				desc = fm["description"]
			}
			tokens += approxTokens(len(strings.TrimSuffix(entry.Name(), ".md")) + len(desc))
		}
	}
	return tokens
}

// censusMCPServers collects MCP server NAMES from the global ~/.claude.json
// `mcpServers` map plus each workspace root's .mcp.json. Only the keys are
// read — commands, URLs, env, and headers stay on the machine.
func censusMCPServers(claudeJSONPath string, workspaceRoots []string) []censusMCPServer {
	seen := map[string]bool{}
	servers := []censusMCPServer{}
	add := func(path string) {
		// #nosec G304 -- path is the global ~/.claude.json or a workspace .mcp.json, not user input; only mcpServers keys are read.
		raw, err := os.ReadFile(path)
		if err != nil {
			return
		}
		var cfg struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return
		}
		for name := range cfg.MCPServers {
			if seen[name] {
				continue
			}
			seen[name] = true
			// Deferred loading is not indicated anywhere in config today.
			servers = append(servers, censusMCPServer{Name: name, Deferred: false})
		}
	}
	add(claudeJSONPath)
	for _, root := range workspaceRoots {
		if root != "" {
			add(filepath.Join(root, ".mcp.json"))
		}
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	return servers
}

// readFrontmatter parses the YAML frontmatter block (--- ... ---) at the top
// of a markdown file into a flat key -> value map. Minimal by design (stdlib
// only): top-level `key: value` pairs, with indented continuation lines and
// block scalars (`>`, `|`) folded into the previous key's value. Returns nil
// when the file is missing or has no frontmatter.
func readFrontmatter(path string) map[string]string {
	// #nosec G304 -- path is a SKILL.md/command/agent markdown file under a discovered plugin install dir, not user input; only frontmatter is parsed.
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(raw), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil
	}
	fm := map[string]string{}
	lastKey := ""
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "---" {
			break
		}
		// Indented line = continuation of the previous value (wrapped string
		// or block-scalar body).
		if lastKey != "" && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
			cont := strings.TrimSpace(line)
			if cont != "" {
				if fm[lastKey] != "" {
					fm[lastKey] += " "
				}
				fm[lastKey] += cont
			}
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		value = strings.TrimSpace(value)
		// Block scalar markers start an empty value filled by continuations.
		if value == ">" || value == ">-" || value == "|" || value == "|-" {
			value = ""
		}
		fm[key] = strings.Trim(value, `"'`)
		lastKey = key
	}
	return fm
}

// --- repo discovery -----------------------------------------------------------

const (
	// censusRepoActivityWindow bounds repo discovery to recently-active repos: a
	// project directory whose newest transcript predates this window is treated as
	// dormant and skipped, so the census reflects where the engineer is actually
	// working, not every repo they ever opened.
	censusRepoActivityWindow = 30 * 24 * time.Hour
	// censusMaxRepos caps how many distinct repos a single census pass emits for,
	// bounding the once-per-24h work (a git call per repo) and the event count.
	// Repos beyond the cap are dropped newest-first-preserving and LOGGED — never
	// silently truncated.
	censusMaxRepos = 64
)

// discoverCensusRepoRoots returns the distinct git-repo roots this device is
// actively capturing for. It is the fix for the "CLAUDE.md coverage = 0%" bug:
// the autostart daemon runs with cwd=$HOME (launchd WorkingDirectory=home, no
// PROMPTSTER_TEAMS_WATCH_DIR), so a census built from the watch root only ever
// described $HOME — a non-repo whose workspaceKey is a bogus sha(HOME) and whose
// projectClaudeMdTokens is structurally 0. Instead we recover the REAL repos the
// same way the transcript watcher does: from the cwd recorded on Claude Code
// transcripts under ~/.claude/projects.
//
// TCC-safe by construction: it reads ONLY files the watcher already sees (the
// transcript store), never a blind WalkDir of $HOME that would fire "wants to
// access your Downloads" consent prompts. Only the recorded cwd is read from
// each transcript (see transcriptCwd) — never its body — and the cwd itself
// never leaves the machine; it is reduced to a repo root and then to a
// workspaceKey (slug or opaque hash).
//
// Bounded: one representative (newest) transcript per project directory, only
// project dirs active within censusRepoActivityWindow, deduped by canonical repo
// root, capped at censusMaxRepos (overflow logged). Returns nil when there is no
// transcript store or no active repo — the caller then emits a single device-only
// census with an empty workspaceKey rather than a fake pseudo-repo.
func discoverCensusRepoRoots() []string {
	entries, err := os.ReadDir(ClaudeProjectsDir())
	if err != nil {
		return nil
	}
	cutoff := time.Now().Add(-censusRepoActivityWindow)

	// One candidate cwd per project directory (Claude Code buckets transcripts by
	// munged cwd), taken from that dir's most-recent transcript. Newest-first so
	// the repo cap keeps the most-recently-active repos.
	type candidate struct {
		cwd string
		mod time.Time
	}
	cands := []candidate{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path, mod := newestTranscript(filepath.Join(ClaudeProjectsDir(), e.Name()))
		if path == "" || mod.Before(cutoff) {
			continue
		}
		cwd := transcriptCwd(path)
		if cwd == "" {
			continue
		}
		cands = append(cands, candidate{cwd: cwd, mod: mod})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })

	seen := map[string]bool{}
	dropped := map[string]bool{}
	roots := []string{}
	for _, c := range cands {
		repo := gitRepoRoot(resolvePath(c.cwd))
		if repo == "" || seen[repo] {
			continue
		}
		if len(roots) >= censusMaxRepos {
			dropped[repo] = true
			continue
		}
		seen[repo] = true
		roots = append(roots, repo)
	}
	if len(dropped) > 0 {
		state.HookDebugf("config census: repo discovery capped at %d; dropped %d additional active repo(s)", censusMaxRepos, len(dropped))
	}
	return roots
}

// newestTranscript returns the path + mtime of the most recently modified
// transcript JSONL under dir (recursively, so subagent sidechains count), or
// ("", zero time) when dir holds none. Stat-only: no file is opened here.
func newestTranscript(dir string) (string, time.Time) {
	var newestPath string
	var newestMod time.Time
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(filepath.Base(path), ".jsonl") {
			return nil
		}
		if info.ModTime().After(newestMod) {
			newestMod = info.ModTime()
			newestPath = path
		}
		return nil
	})
	return newestPath, newestMod
}

// transcriptCwd returns the cwd recorded on the first cwd-bearing line of a
// Claude Code transcript, or "" when none appears in the early lines. It mirrors
// classifyClaudeTranscript's scan (first lines only, `cwd` field only) — the
// transcript body is never retained, only the working directory, which the
// caller reduces to a privacy-safe repo identity.
func transcriptCwd(path string) string {
	// #nosec G304 -- path is a Claude transcript discovered under ~/.claude/projects, not user input; opened read-only and only the cwd field is read.
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	const maxScanLines = 50
	scanned := 0
	for scanner.Scan() {
		scanned++
		if scanned > maxScanLines {
			return ""
		}
		var rec struct {
			Cwd string `json:"cwd"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}
		if rec.Cwd != "" {
			return rec.Cwd
		}
	}
	return ""
}

// --- emission ----------------------------------------------------------------

// lastCensusAtPath persists when the last census was emitted, so restarts and
// hourly ticks don't re-emit inside the 24h window (startup always emits; the
// cursor only paces the ticker).
func lastCensusAtPath() string {
	return filepath.Join(state.GlobalPromptsterDir(), "last-census-at")
}

func loadLastCensusAt() time.Time {
	raw, err := os.ReadFile(lastCensusAtPath())
	if err != nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(raw)))
	if err != nil {
		return time.Time{}
	}
	return t
}

func saveLastCensusAt(t time.Time) {
	p := lastCensusAtPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = os.WriteFile(p, []byte(t.UTC().Format(time.RFC3339)), 0o600)
}

// buildConfigCensusEventForWorkspace wraps the census for a single workspace in
// the ordinary event envelope so it is signed and chained exactly like every
// other event. workspace "" yields a device-only census (empty workspaceKey,
// projectClaudeMdTokens 0) — the honest "no active repo" shape the backend
// coverage fold skips.
//
// The census goes through eventDataMap rather than being assigned directly:
// Data must hold a map[string]interface{} or the redaction projector
// default-denies it and the whole census ships as {}. See eventDataMap.
// Like presence, the census is DEVICE-scoped — it describes the machine's tool
// configuration, not any one AI-tool session — so its envelope sessionId stays
// the device id and the backend skips minting a session row for this kind. When
// a device captures for several repos it emits several censuses, each with the
// same device inventory but a distinct workspaceKey; the backend folds them by
// workspaceKey (coverage), unions skills per key (idempotent), and reads only
// the newest for MCP — so N censuses per device is correct with no schema change.
func buildConfigCensusEventForWorkspace(session Session, workspace string) event.Event {
	e := event.NewEvent("config_census", session.DeviceID)
	e.Source = presenceSource
	e.DeviceID = session.DeviceID
	e.Actor = event.SystemActor()
	e.Data = eventDataMap(buildConfigCensus(defaultCensusEnv(workspace)))
	return e
}

// buildConfigCensusEvent builds the single census for the session's own workspace
// (session.TaskRoot). Retained for the emit-path tests; the fleet emitter uses
// the per-workspace builder directly over the discovered repos.
func buildConfigCensusEvent(session Session) event.Event {
	return buildConfigCensusEventForWorkspace(session, session.TaskRoot)
}

// buildConfigCensusEvents builds the census events this device should emit: one
// per distinct real repo it is actively capturing for (each carrying the full
// device inventory plus that repo's workspaceKey + projectClaudeMdTokens), or a
// single device-only census with an EMPTY workspaceKey when no repo is active.
//
// Emitting the empty-workspaceKey census (rather than the old $HOME-rooted one)
// is deliberate: the backend coverage fold skips empty keys, so a fresh device
// with no sessions yet reports an honest "nothing to grade yet" instead of the
// fake sha(HOME)/0 pseudo-repo that structurally pinned CLAUDE.md coverage to 0%.
func buildConfigCensusEvents(session Session) []event.Event {
	roots := discoverCensusRepoRoots()
	if len(roots) == 0 {
		return []event.Event{buildConfigCensusEventForWorkspace(session, "")}
	}
	events := make([]event.Event, 0, len(roots))
	for _, root := range roots {
		events = append(events, buildConfigCensusEventForWorkspace(session, root))
	}
	return events
}

// appendConfigCensus runs one prepared census event through the SAME
// buffer/sign/queue funnel as captured events.
//
// The census is QUEUED rather than POSTed inline, unlike the presence heartbeat
// next door. It is emitted at most once per 24h and the cursor
// (saveLastCensusAt) advances whether or not the send worked — so an inline
// POST that hit a 429 or a backend blip silently lost the whole census for a
// day, and fleet-health's "no census" signal would fire for a device that had
// dutifully collected one. That is bug 2's exact shape: a cursor advancing past
// an event the network dropped. A rare, expensive, non-time-sensitive event is
// precisely what a durable queue is for.
//
// Still 400-tolerant: a backend that doesn't accept config_census yet rejects
// it with a 4xx and the drain skips it (see outbox.deliver), so a rejecting
// backend is still probed at most once per interval.
func appendConfigCensus(ev event.Event) {
	// captureAssistantProse=false: a config_census event carries no ai_response
	// text, so the prose gate is irrelevant — pass the fail-closed default.
	if err := sign.AppendEventToLocalBuffer(&ev, false); err != nil {
		state.HookDebugf("config census buffer error: %v", err)
	}
	if err := outbox.Append(ev); err != nil {
		state.HookDebugf("config census queue error: %v", err)
	}
}

// emitConfigCensus discovers the repos this device captures for and queues one
// config_census per repo (or a single device-only census when none are active),
// then advances the once-per-24h cursor. The cursor advances once for the whole
// pass — a discovery that finds N repos is still one census "tick".
func emitConfigCensus(session Session) {
	for _, ev := range buildConfigCensusEvents(session) {
		appendConfigCensus(ev)
	}
	saveLastCensusAt(time.Now())
}

// runConfigCensus emits one census immediately, then re-emits whenever 24h
// have elapsed since the persisted cursor, until stop is closed.
func runConfigCensus(session Session, stop <-chan struct{}) {
	emitConfigCensus(session)
	ticker := time.NewTicker(configCensusCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if time.Since(loadLastCensusAt()) >= configCensusInterval {
				emitConfigCensus(session)
			}
		}
	}
}

// startConfigCensus launches the census goroutine and returns a stop function
// the caller defers. Mirrors startPresenceHeartbeat.
func StartConfigCensus(session Session) (stop func()) {
	done := make(chan struct{})
	go runConfigCensus(session, done)
	return func() { close(done) }
}
