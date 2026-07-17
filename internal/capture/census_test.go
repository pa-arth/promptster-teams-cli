package capture

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCensusFixture builds a fake Claude config dir + workspace under
// t.TempDir() and returns the censusEnv pointing at it.
func writeCensusFixture(t *testing.T) censusEnv {
	t.Helper()
	tmp := t.TempDir()
	claudeDir := filepath.Join(tmp, "claude")
	ws := filepath.Join(tmp, "ws")

	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(tmp, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Global CLAUDE.md: 400 chars → 100 tokens.
	write("claude/CLAUDE.md", strings.Repeat("g", 400))
	// Workspace CLAUDE.md: 80 chars → 20 tokens.
	write("ws/CLAUDE.md", strings.Repeat("w", 80))

	// Skill with single-line description (40 chars → 10 tokens).
	write("claude/skills/deploy-check/SKILL.md",
		"---\nname: deploy-check\ndescription: "+strings.Repeat("d", 40)+"\n---\n\n# Deploy Check\n\nSECRET-SKILL-BODY should never leave the machine.\n")
	// Skill with a wrapped (multiline) description: 20 + 1 (join space) + 19
	// chars → 40 chars → 10 tokens; name falls back to the slug.
	write("claude/skills/wrapped/SKILL.md",
		"---\ndescription: "+strings.Repeat("a", 20)+"\n  "+strings.Repeat("b", 19)+"\n---\nbody\n")
	// A non-skill file in the skills dir is ignored.
	write("claude/skills/notes.md", "not a skill")

	// Enabled plugins (one with an enumerable install dir, one without).
	write("claude/settings.json", `{"enabledPlugins":{"listed@mkt":true,"ghost@mkt":true,"disabled@mkt":false}}`)
	pluginDir := filepath.Join(claudeDir, "plugins", "cache", "mkt", "listed")
	registry := `{"version":2,"plugins":{"listed@mkt":[{"installPath":` + jsonString(pluginDir) + `}]}}`
	write("claude/plugins/installed_plugins.json", registry)
	// Plugin skill: name "sk" (2 chars) + 38-char description → 40 chars → 10 tokens.
	write("claude/plugins/cache/mkt/listed/skills/sk/SKILL.md",
		"---\nname: sk\ndescription: "+strings.Repeat("p", 38)+"\n---\nplugin skill body\n")
	// Plugin command: name "cmd" (3) + 37-char description → 40 chars → 10 tokens.
	write("claude/plugins/cache/mkt/listed/commands/cmd.md",
		"---\ndescription: "+strings.Repeat("c", 37)+"\n---\ncommand body\n")

	// Global MCP registry + workspace .mcp.json (with one duplicate name).
	write("claude.json", `{"mcpServers":{"posthog":{"type":"http","url":"https://x"},"supabase":{"type":"stdio","command":"npx","env":{"TOKEN":"SECRET-MCP-ENV"}}}}`)
	write("ws/.mcp.json", `{"mcpServers":{"posthog":{"type":"http"},"local-only":{"type":"stdio"}}}`)

	return censusEnv{
		claudeDir:      claudeDir,
		claudeJSONPath: filepath.Join(tmp, "claude.json"),
		workspaceRoots: []string{ws, ws}, // duplicate root must not double-count
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestBuildConfigCensusFromFixture(t *testing.T) {
	data := buildConfigCensus(writeCensusFixture(t))

	if data.GlobalClaudeMdTokens != 100 {
		t.Errorf("globalClaudeMdTokens = %d", data.GlobalClaudeMdTokens)
	}
	if data.ProjectClaudeMdTokens != 20 {
		t.Errorf("projectClaudeMdTokens = %d", data.ProjectClaudeMdTokens)
	}

	if data.SkillCount != 2 || len(data.Skills) != 2 {
		t.Fatalf("skills = %+v", data.Skills)
	}
	if data.Skills[0].Slug != "deploy-check" || data.Skills[0].Name != "deploy-check" || data.Skills[0].DescTokens != 10 {
		t.Errorf("skill[0] = %+v", data.Skills[0])
	}
	// Frontmatter without name: falls back to slug; wrapped description folds
	// to 40 chars → 10 tokens.
	if data.Skills[1].Slug != "wrapped" || data.Skills[1].Name != "wrapped" || data.Skills[1].DescTokens != 10 {
		t.Errorf("skill[1] = %+v", data.Skills[1])
	}
	if data.SkillListingTokens != 20 {
		t.Errorf("skillListingTokens = %d", data.SkillListingTokens)
	}

	if data.PluginCount != 2 || len(data.Plugins) != 2 {
		t.Fatalf("plugins = %+v", data.Plugins)
	}
	// Sorted: ghost@mkt (no install dir → 0), listed@mkt (skill 10 + command 10).
	if data.Plugins[0].Name != "ghost@mkt" || data.Plugins[0].ListingTokens != 0 {
		t.Errorf("plugin[0] = %+v", data.Plugins[0])
	}
	if data.Plugins[1].Name != "listed@mkt" || data.Plugins[1].ListingTokens != 20 {
		t.Errorf("plugin[1] = %+v", data.Plugins[1])
	}
	if data.PluginListingTokens != 20 {
		t.Errorf("pluginListingTokens = %d", data.PluginListingTokens)
	}

	if len(data.MCPServers) != 3 {
		t.Fatalf("mcpServers = %+v", data.MCPServers)
	}
	names := []string{data.MCPServers[0].Name, data.MCPServers[1].Name, data.MCPServers[2].Name}
	if names[0] != "local-only" || names[1] != "posthog" || names[2] != "supabase" {
		t.Errorf("mcp names = %v", names)
	}
	for _, s := range data.MCPServers {
		if s.Deferred {
			t.Errorf("deferred must be false (not detectable): %+v", s)
		}
	}
	if data.MCPDeferred {
		t.Error("mcpDeferred must be false")
	}
}

// TestConfigCensusCarriesNoFileContents pins the census privacy rule: the
// serialized payload may carry names and token counts, never the contents of
// CLAUDE.md, skill bodies, plugin sources, or MCP server config values.
func TestConfigCensusCarriesNoFileContents(t *testing.T) {
	env := writeCensusFixture(t)

	// Point the transcript store at a fixture holding a distinctively-named
	// slug dir + transcript filename, so we can assert those path-like strings
	// never survive into the payload — the capture-health counts must ship as
	// integers, not names.
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	transcript := filepath.Join(cfgDir, "projects", "SECRET-CAPTURE-SLUG", "SECRET-TRANSCRIPT.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte(`{"secret":"transcript body"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	data := buildConfigCensus(env)
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	payload := string(raw)

	// Sanity: the transcript store must actually have been counted, or the
	// path-leak assertions below would be vacuous.
	if data.ClaudeTranscriptsTotal == nil || *data.ClaudeTranscriptsTotal < 1 {
		t.Fatalf("expected transcript store to be counted, got total=%v", data.ClaudeTranscriptsTotal)
	}
	for _, leak := range []string{
		"SECRET-SKILL-BODY",     // skill body
		"SECRET-MCP-ENV",        // MCP env value
		strings.Repeat("g", 40), // CLAUDE.md content
		strings.Repeat("d", 40), // skill description VALUE (only its length may ship)
		"https://x",             // MCP server URL
		"npx",                   // MCP server command
		"SECRET-CAPTURE-SLUG",   // transcript store slug/dir name
		"SECRET-TRANSCRIPT",     // transcript filename
		".jsonl",                // no transcript path may ship
		"projects",              // no transcript store path may ship
	} {
		if strings.Contains(payload, leak) {
			t.Errorf("census payload leaks content %q: %s", leak, payload)
		}
	}
	// The workspace key must never carry the workspace filesystem path (the
	// non-git fixture falls back to a hash, which must be opaque).
	if abs, err := filepath.Abs(env.workspaceRoots[0]); err == nil && strings.Contains(payload, abs) {
		t.Errorf("census payload leaks workspace path %q: %s", abs, payload)
	}
	// The closed field set — nothing beyond counts and names.
	var round map[string]interface{}
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"workspaceKey",
		"globalClaudeMdTokens", "projectClaudeMdTokens",
		"skills", "skillListingTokens", "skillCount",
		"plugins", "pluginListingTokens", "pluginCount",
		"mcpServers", "mcpDeferred",
		"claudeTranscriptsTotal", "claudeTranscriptsActive7d",
	}
	if len(round) != len(want) {
		t.Errorf("census shape changed: %v", round)
	}
	for _, k := range want {
		if _, ok := round[k]; !ok {
			t.Errorf("missing field %s", k)
		}
	}
}

// TestCountClaudeTranscripts pins the capture-health counter: total .jsonl
// files under ClaudeProjectsDir() (recursively, subagent sidechains included),
// and how many are active within the last 7 days. Missing dir → (0, 0).
func TestCountClaudeTranscripts(t *testing.T) {
	// Missing projects dir (config dir exists but has no projects/) → (0, 0).
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if total, active := countClaudeTranscripts(); total == nil || active == nil || *total != 0 || *active != 0 {
		t.Errorf("missing projects dir must yield a definite (0, 0), got %v/%v", total, active)
	}

	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	projects := filepath.Join(cfg, "projects")

	mk := func(rel string) string {
		t.Helper()
		p := filepath.Join(projects, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Two recent transcripts + a nested subagent sidechain (all active),
	// one old transcript, plus a non-jsonl file that must be ignored.
	mk("-repo-a/session1.jsonl")
	mk("-repo-a/session2.jsonl")
	mk("-repo-a/session3/subagents/agent-x.jsonl") // nested → counts recursively
	old := mk("-repo-b/old.jsonl")
	mk("-repo-a/notes.txt") // not a transcript → ignored

	// Age the old transcript out of the 7-day window.
	past := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	total, active := countClaudeTranscripts()
	if total == nil || *total != 4 {
		t.Errorf("total = %v, want 4 (three recent + one old, .txt ignored)", total)
	}
	if active == nil || *active != 3 {
		t.Errorf("active7d = %v, want 3 (old.jsonl excluded)", active)
	}
}

// TestCountClaudeTranscriptsUnreadableTreeIsUnknown pins the P1 fix: an
// unreadable projects subtree must yield (nil, nil) — omitted from the census →
// the backend reads UNKNOWN — never a false low count that would read as "not
// using Claude Code locally" when capture simply can't see the files.
func TestCountClaudeTranscriptsUnreadableTreeIsUnknown(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions; cannot simulate an unreadable subtree")
	}
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	blocked := filepath.Join(cfg, "projects", "-repo-locked")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "s.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) }) // let TempDir cleanup remove it

	total, active := countClaudeTranscripts()
	if total != nil || active != nil {
		t.Errorf("unreadable subtree must yield (nil, nil) = unknown, got %v/%v", total, active)
	}
}

// TestConfigCensusWorkspaceKey pins the non-git fallback: an opaque, stable,
// path-free hash that is populated, repeatable, and distinct per workspace.
func TestConfigCensusWorkspaceKey(t *testing.T) {
	env := writeCensusFixture(t) // fixture ws is not a git repo → hash fallback

	a := buildConfigCensus(env).WorkspaceKey
	if a == "" {
		t.Fatal("workspaceKey must be populated")
	}
	if b := buildConfigCensus(env).WorkspaceKey; a != b {
		t.Errorf("workspaceKey must be stable across builds: %q != %q", a, b)
	}
	if len(a) != 16 {
		t.Errorf("hashed workspaceKey must be 16 hex chars, got %q (len %d)", a, len(a))
	}
	if strings.Contains(a, string(filepath.Separator)) || strings.Contains(a, env.workspaceRoots[0]) {
		t.Errorf("workspaceKey must not leak a path: %q", a)
	}

	// A different workspace must produce a different key.
	if other := buildConfigCensus(writeCensusFixture(t)).WorkspaceKey; other == a {
		t.Errorf("distinct workspaces must yield distinct keys, both %q", a)
	}
}

// TestWorkspaceKeyPrefersGitRemoteSlug pins the preferred path: a git repo with
// an origin remote reports its owner/name slug (matching outcome_events.repo),
// not a hash.
func TestWorkspaceKeyPrefersGitRemoteSlug(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ws := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", ws}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	git("remote", "add", "origin", "git@github.com:promptster/teams-cli.git")

	if got := workspaceKey(ws); got != "promptster/teams-cli" {
		t.Errorf("workspaceKey = %q, want git slug promptster/teams-cli", got)
	}
}

// TestNormalizeRemoteSlug covers the git URL forms the slug normalizer must
// reduce to owner/name, and the cases it must reject.
func TestNormalizeRemoteSlug(t *testing.T) {
	cases := map[string]string{
		"git@github.com:owner/name.git":         "owner/name",
		"git@github.com:owner/name":             "owner/name",
		"https://github.com/owner/name.git":     "owner/name",
		"https://github.com/owner/name":         "owner/name",
		"ssh://git@github.com/owner/name.git":   "owner/name",
		"https://gitlab.com/group/sub/name.git": "sub/name", // nested → last two
		"  git@github.com:owner/name.git\n":     "owner/name",
		"git@internal-host:owner/name":          "owner/name", // bare host, no dot
		"":                                      "",
		"not-a-url":                             "",
		// Local-path remotes must NEVER leak filesystem segments into the slug —
		// each of these reduces to owner/name today only because it splits a path.
		"/home/alice/repos/myproject":  "",
		"/home/alice/myproject":        "",
		"./myproject":                  "",
		"file:///home/alice/myproject": "",
		"C:/Users/alice/repo":          "",
	}
	for in, want := range cases {
		if got := normalizeRemoteSlug(in); got != want {
			t.Errorf("normalizeRemoteSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildConfigCensusMissingEverything(t *testing.T) {
	tmp := t.TempDir()
	data := buildConfigCensus(censusEnv{
		claudeDir:      filepath.Join(tmp, "nope"),
		claudeJSONPath: filepath.Join(tmp, "nope.json"),
		workspaceRoots: []string{filepath.Join(tmp, "nows"), ""},
	})
	if data.GlobalClaudeMdTokens != 0 || data.ProjectClaudeMdTokens != 0 ||
		data.SkillCount != 0 || data.PluginCount != 0 || len(data.MCPServers) != 0 {
		t.Errorf("empty env must produce zeros: %+v", data)
	}
	// Arrays must be present (not null) so the payload shape is stable.
	raw, _ := json.Marshal(data)
	if strings.Contains(string(raw), "null") {
		t.Errorf("empty census must not serialize null arrays: %s", raw)
	}
}

// TestCensusPluginsEnabledDisabledFallback pins the nil-vs-empty contract:
// the installed-plugin fallback only applies when there is no authoritative
// enabledPlugins list at all — never when the user explicitly disabled
// every plugin.
func TestCensusPluginsEnabledDisabledFallback(t *testing.T) {
	setup := func(t *testing.T, settingsJSON string) string {
		t.Helper()
		claudeDir := t.TempDir()
		pluginDir := filepath.Join(claudeDir, "plugins", "cache", "mkt", "a")
		registry := `{"version":2,"plugins":{"a@mkt":[{"installPath":` + jsonString(pluginDir) + `}],"b@mkt":[{"installPath":` + jsonString(pluginDir) + `}]}}`
		if err := os.MkdirAll(filepath.Join(claudeDir, "plugins"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(claudeDir, "plugins", "installed_plugins.json"), []byte(registry), 0o644); err != nil {
			t.Fatal(err)
		}
		if settingsJSON != "" {
			if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(settingsJSON), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return claudeDir
	}

	// (a) Every plugin explicitly disabled → zero plugins, no fallback.
	if got := censusPlugins(setup(t, `{"enabledPlugins":{"a@mkt":false,"b@mkt":false}}`)); len(got) != 0 {
		t.Errorf("all-disabled must report zero plugins, got %+v", got)
	}
	// (b) settings.json present but no enabledPlugins key → fall back to all installed.
	if got := censusPlugins(setup(t, `{"model":"opus"}`)); len(got) != 2 {
		t.Errorf("missing enabledPlugins key must fall back to installed, got %+v", got)
	}
	// (c) settings.json missing entirely → fall back to all installed.
	if got := censusPlugins(setup(t, "")); len(got) != 2 {
		t.Errorf("missing settings.json must fall back to installed, got %+v", got)
	}
}

func TestReadFrontmatter(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "SKILL.md")
	content := "---\nname: my-skill\ndescription: >-\n  first part\n  second part\nother: \"quoted\"\n---\n# Body\nnot: frontmatter\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	fm := readFrontmatter(p)
	if fm == nil {
		t.Fatal("nil frontmatter")
	}
	if fm["name"] != "my-skill" {
		t.Errorf("name = %q", fm["name"])
	}
	if fm["description"] != "first part second part" {
		t.Errorf("description = %q", fm["description"])
	}
	if fm["other"] != "quoted" {
		t.Errorf("other = %q", fm["other"])
	}
	if _, has := fm["not"]; has {
		t.Error("body keys must not parse")
	}
	// No frontmatter → nil.
	p2 := filepath.Join(tmp, "plain.md")
	_ = os.WriteFile(p2, []byte("# just markdown\n"), 0o644)
	if readFrontmatter(p2) != nil {
		t.Error("plain markdown must return nil")
	}
	if readFrontmatter(filepath.Join(tmp, "missing.md")) != nil {
		t.Error("missing file must return nil")
	}
}

// writeClaudeFixture writes a CLAUDE.md of `chars` bytes at `rel` under root.
func writeClaudeFixture(t *testing.T, root, rel string, chars int) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Repeat("x", chars)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestProjectClaudeMdTokensNested pins the fix for the cc-audit "CLAUDE.md
// coverage = 0%" bug: when the repo keeps its memory in a sub-package (no root
// CLAUDE.md), the census falls back to the LARGEST nested file rather than 0.
// Siblings are NOT summed (a request under one package doesn't load another's),
// and dependency/build trees, hidden dirs (.git, .claude/worktrees), and
// anything past the depth bound must NOT contribute.
func TestProjectClaudeMdTokensNested(t *testing.T) {
	root := t.TempDir()

	// No CLAUDE.md at the root — the exact monorepo shape (e.g.
	// my-clerk-next-app/CLAUDE.md) that used to score 0%.
	writeClaudeFixture(t, root, "app/CLAUDE.md", 200)         // 50 tokens (the max)
	writeClaudeFixture(t, root, "packages/x/y/CLAUDE.md", 40) // 10 tokens — a sibling, must NOT add

	// Must all be ignored: dependency/build/vendor trees, hidden dirs (VCS +
	// this repo's own .claude/worktrees copies), and beyond the depth bound.
	writeClaudeFixture(t, root, "node_modules/dep/CLAUDE.md", 4000)
	writeClaudeFixture(t, root, "vendor/lib/CLAUDE.md", 4000)
	writeClaudeFixture(t, root, "dist/CLAUDE.md", 4000)
	writeClaudeFixture(t, root, ".git/CLAUDE.md", 4000)
	writeClaudeFixture(t, root, ".claude/worktrees/wt/CLAUDE.md", 4000)
	writeClaudeFixture(t, root, "d1/d2/d3/d4/d5/d6/CLAUDE.md", 4000) // dir d6 is 6 levels down → past bound

	const want = 50 // max(app=50, packages/x/y=10), NOT the 60 sum
	if got := projectClaudeMdTokens([]string{root}); got != want {
		t.Errorf("projectClaudeMdTokens = %d, want %d (largest nested, not summed)", got, want)
	}

	// Overlapping roots (a workspace + a sub-root resolving into the same tree)
	// must not inflate — max is idempotent.
	if got := projectClaudeMdTokens([]string{root, root, filepath.Join(root, "app")}); got != want {
		t.Errorf("overlapping roots inflated: got %d, want %d", got, want)
	}

	// Empty / missing roots contribute nothing and never error.
	if got := projectClaudeMdTokens([]string{"", filepath.Join(root, "does-not-exist")}); got != 0 {
		t.Errorf("empty/missing roots = %d, want 0", got)
	}
}

// TestProjectClaudeMdTokensRootPreferred pins the always-loaded semantic: when a
// root CLAUDE.md exists, it is the reported value and nested sibling packages are
// ignored entirely (they aren't loaded on every request, so they must not inflate
// the config-tax figure). This also guarantees repos that already worked keep
// their exact prior number.
func TestProjectClaudeMdTokensRootPreferred(t *testing.T) {
	root := t.TempDir()
	writeClaudeFixture(t, root, "CLAUDE.md", 80)              // 20 tokens — always-loaded root
	writeClaudeFixture(t, root, "packages/a/CLAUDE.md", 4000) // huge nested sibling, must be ignored

	const want = 20
	if got := projectClaudeMdTokens([]string{root}); got != want {
		t.Errorf("projectClaudeMdTokens = %d, want %d (root only, nested ignored)", got, want)
	}
}

// TestProjectClaudeMdTokensDepthBound pins the depth boundary: a CLAUDE.md
// exactly claudeMdMaxDepth (5) levels below the root is INCLUDED; one level
// deeper is excluded. Guards the off-by-one that would drop the documented
// boundary layout.
func TestProjectClaudeMdTokensDepthBound(t *testing.T) {
	// Depth 5 (l1/l2/l3/l4/l5/CLAUDE.md): dir l5 is 5 levels below root → included.
	inRoot := t.TempDir()
	writeClaudeFixture(t, inRoot, "l1/l2/l3/l4/l5/CLAUDE.md", 200)
	if got := projectClaudeMdTokens([]string{inRoot}); got != 50 {
		t.Errorf("depth-5 file: got %d, want 50 (must be included)", got)
	}

	// Depth 6 (m1/.../m6/CLAUDE.md): dir m6 is 6 levels below root → excluded.
	outRoot := t.TempDir()
	writeClaudeFixture(t, outRoot, "m1/m2/m3/m4/m5/m6/CLAUDE.md", 200)
	if got := projectClaudeMdTokens([]string{outRoot}); got != 0 {
		t.Errorf("depth-6 file: got %d, want 0 (must be excluded)", got)
	}
}
