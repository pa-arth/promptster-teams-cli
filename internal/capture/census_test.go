package capture

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
	data := buildConfigCensus(env)
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	payload := string(raw)
	for _, leak := range []string{
		"SECRET-SKILL-BODY",     // skill body
		"SECRET-MCP-ENV",        // MCP env value
		strings.Repeat("g", 40), // CLAUDE.md content
		strings.Repeat("d", 40), // skill description VALUE (only its length may ship)
		"https://x",             // MCP server URL
		"npx",                   // MCP server command
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
