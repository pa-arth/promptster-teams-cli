package capture

import (
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
	WorkspaceKey          string            `json:"workspaceKey"`
	GlobalClaudeMdTokens  int               `json:"globalClaudeMdTokens"`
	ProjectClaudeMdTokens int               `json:"projectClaudeMdTokens"`
	Skills                []censusSkill     `json:"skills"`
	SkillListingTokens    int               `json:"skillListingTokens"`
	SkillCount            int               `json:"skillCount"`
	Plugins               []censusPlugin    `json:"plugins"`
	PluginListingTokens   int               `json:"pluginListingTokens"`
	PluginCount           int               `json:"pluginCount"`
	MCPServers            []censusMCPServer `json:"mcpServers"`
	MCPDeferred           bool              `json:"mcpDeferred"`
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

// claudeMdMaxDepth bounds how deep the project-CLAUDE.md scan descends below a
// workspace root. Claude Code discovers CLAUDE.md HIERARCHICALLY (a repo may
// keep its memory in a sub-package, e.g. my-clerk-next-app/CLAUDE.md), so the
// census must too — but an unbounded walk on a large monorepo is a cost this
// once-per-24h census should not pay. Sub-package CLAUDE.md files sit within a
// few levels; 5 covers the real layouts (apps/api/CLAUDE.md, packages/x/CLAUDE.md)
// with margin. Depth is counted from the root: root/CLAUDE.md is depth 0.
const claudeMdMaxDepth = 5

// projectClaudeMdSkipDirs are directory names the scan never descends into: a
// CLAUDE.md inside a dependency/build/vendor tree belongs to a third party, not
// to this workspace. Hidden directories (".*") are skipped separately by the
// walker — that also excludes .git and, crucially, .claude/worktrees, whose
// nested repo copies would otherwise be counted many times over.
var projectClaudeMdSkipDirs = map[string]bool{
	"node_modules": true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
	"out":          true,
	"target":       true,
	"__pycache__":  true,
}

// projectClaudeMdTokens sums the token cost of every project CLAUDE.md Claude
// Code would load for work anywhere under the given workspace roots, matching
// its hierarchical discovery: a CLAUDE.md at the root OR nested in a sub-package
// both count. The old behavior checked only <root>/CLAUDE.md, so it reported 0
// tokens for every monorepo that keeps its CLAUDE.md in a sub-directory — which
// read downstream as "no project CLAUDE.md" and scored the whole workspace 0% on
// the cc-audit coverage check.
//
// Files are de-duped by absolute path so overlapping roots (a workspace and a
// worktree that resolve into the same tree) can never double-count. Content is
// never read — fileTokens stats size only, preserving the no-file-contents
// guarantee. Every branch is best-effort: an unreadable subtree is skipped, not
// surfaced as an error.
func projectClaudeMdTokens(roots []string) int {
	seenFiles := map[string]bool{}
	seenRoots := map[string]bool{}
	total := 0
	for _, root := range roots {
		if root == "" || seenRoots[root] {
			continue
		}
		seenRoots[root] = true
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
				if strings.Count(filepath.Clean(path), string(os.PathSeparator))-rootDepth >= claudeMdMaxDepth {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Name() != "CLAUDE.md" {
				return nil
			}
			abs := path
			if a, absErr := filepath.Abs(path); absErr == nil {
				abs = a
			}
			if seenFiles[abs] {
				return nil
			}
			seenFiles[abs] = true
			total += fileTokens(path)
			return nil
		})
	}
	return total
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
// falls back to an opaque sha256(abspath) hash truncated to 16 hex chars. The
// result never carries a filesystem path or file contents. Empty root → "".
func workspaceKey(root string) string {
	if root == "" {
		return ""
	}
	if slug := gitRemoteSlug(root); slug != "" {
		return slug
	}
	abs := root
	if a, err := filepath.Abs(root); err == nil {
		abs = a
	}
	return ingest.Sha256Hex(abs)[:16]
}

// gitRemoteSlug returns the workspace's origin remote as an owner/name slug, or
// "" when the dir is not a git repo, has no origin remote, or the URL can't be
// reduced to owner/name. Only the slug leaves the machine — never the URL.
func gitRemoteSlug(root string) string {
	// A local `git config` read is normally milliseconds, but on a
	// network-mounted workspace or a corrupt .git it can hang indefinitely —
	// bound it so census never stalls the watch process.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// #nosec G204 -- constant argv; root is a discovered workspace dir, not user input. Reads only the local origin URL, timeout-bounded.
	out, err := exec.CommandContext(ctx, "git", "-C", root, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return ""
	}
	return normalizeRemoteSlug(string(out))
}

// normalizeRemoteSlug reduces a git remote URL to its trailing owner/name,
// stripping scheme, host, userinfo, and a trailing ".git". Handles the common
// forms: https://host/owner/name(.git), ssh://git@host/owner/name(.git), and
// the scp-style git@host:owner/name(.git). Returns "" when it can't isolate an
// owner and name. Taking only the last two path segments guarantees no full
// filesystem path can survive into the identity.
func normalizeRemoteSlug(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	raw = strings.TrimSuffix(raw, ".git")
	if i := strings.Index(raw, "://"); i >= 0 {
		// scheme://[user@]host/owner/name — drop scheme + host. A non-empty host
		// must sit between the "://" and the first "/"; an empty host (as in
		// file:///home/alice/repo) means a local path, not a hosted remote —
		// reject so no filesystem segment can survive into the identity.
		if strings.EqualFold(raw[:i], "file") {
			return ""
		}
		rest := raw[i+3:]
		j := strings.Index(rest, "/")
		if j <= 0 {
			return ""
		}
		raw = rest[j+1:]
	} else if i := strings.LastIndex(raw, ":"); i >= 0 {
		// scp-style [user@]host:owner/name — everything after the colon. Reject
		// forms that are actually filesystem paths: a host with a slash, or a
		// path after the colon (C:/Users/alice/repo, git@host:/abs/path).
		host, rest := raw[:i], raw[i+1:]
		if host == "" || strings.ContainsAny(host, "/\\") || strings.HasPrefix(rest, "/") {
			return ""
		}
		raw = rest
	} else {
		// No scheme and no colon → a bare local path (/home/alice/repo, ./repo)
		// or junk, never a hosted remote. Reject so the WorkspaceKey never
		// encodes a filesystem path (it falls back to the hashed root instead).
		return ""
	}
	parts := strings.Split(strings.Trim(raw, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	owner, name := parts[len(parts)-2], parts[len(parts)-1]
	if owner == "" || name == "" {
		return ""
	}
	return owner + "/" + name
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

	// Project CLAUDE.md files across the watched workspace roots, summed.
	data.ProjectClaudeMdTokens = projectClaudeMdTokens(env.workspaceRoots)

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

// buildConfigCensusEvent wraps the census in the ordinary event envelope so it
// is signed and chained exactly like every other event.
//
// The census goes through eventDataMap rather than being assigned directly:
// Data must hold a map[string]interface{} or the redaction projector
// default-denies it and the whole census ships as {}. See eventDataMap.
// Like presence, the census is DEVICE-scoped — it describes the machine's tool
// configuration, not any one AI-tool session — so its envelope sessionId stays
// the device id and the backend skips minting a session row for this kind.
func buildConfigCensusEvent(session Session) event.Event {
	e := event.NewEvent("config_census", session.DeviceID)
	e.Source = presenceSource
	e.DeviceID = session.DeviceID
	e.Actor = event.SystemActor()
	e.Data = eventDataMap(buildConfigCensus(defaultCensusEnv(session.TaskRoot)))
	return e
}

// emitConfigCensus builds one census and runs it through the SAME
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
func emitConfigCensus(session Session) {
	ev := buildConfigCensusEvent(session)
	// captureAssistantProse=false: a config_census event carries no ai_response
	// text, so the prose gate is irrelevant — pass the fail-closed default.
	if err := sign.AppendEventToLocalBuffer(&ev, false); err != nil {
		state.HookDebugf("config census buffer error: %v", err)
	}
	if err := outbox.Append(ev); err != nil {
		state.HookDebugf("config census queue error: %v", err)
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
