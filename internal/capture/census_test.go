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

// TestNormalizeRemoteHostAndSlug covers the host half of the split. The slug
// column repeats TestNormalizeRemoteSlug's expectations ON PURPOSE: the whole
// safety argument for splitting normalizeRemoteSlug is that its output did not
// move, so every case asserts both halves together.
func TestNormalizeRemoteHostAndSlug(t *testing.T) {
	cases := map[string]struct{ host, slug string }{
		// The three mainstream forms, across three providers. Same slug shape,
		// different hosts — this is the ambiguity the host exists to resolve.
		"git@github.com:owner/name.git":         {"github.com", "owner/name"},
		"https://github.com/owner/name.git":     {"github.com", "owner/name"},
		"ssh://git@github.com/owner/name.git":   {"github.com", "owner/name"},
		"https://gitlab.com/owner/name.git":     {"gitlab.com", "owner/name"},
		"git@bitbucket.org:owner/name.git":      {"bitbucket.org", "owner/name"},
		"https://gitlab.com/group/sub/name.git": {"gitlab.com", "sub/name"},
		// Self-hosted: a bare host with no dot is still a host.
		"git@internal-host:owner/name":         {"internal-host", "owner/name"},
		"https://git.acme.internal/owner/name": {"git.acme.internal", "owner/name"},

		// Host normalization. A host is only useful if it compares equal to the
		// backend's provider string, so case, userinfo and port must not survive.
		"https://GitHub.COM/owner/name":          {"github.com", "owner/name"},
		"ssh://git@github.com:22/owner/name.git": {"github.com", "owner/name"},
		"https://user:pw@github.com/owner/name":  {"github.com", "owner/name"},
		"https://tok@n@github.com/owner/name":    {"github.com", "owner/name"},
		"ssh://git@[2001:db8::1]:22/owner/name":  {"[2001:db8::1]", "owner/name"},
		"https://github.com:443/owner/name.git":  {"github.com", "owner/name"},

		// scp-style splits on the FIRST colon, not the last. The scp form has no port
		// syntax, so any later colon belongs to the PATH. Splitting late would take
		// "git@host:a" as the authority — yielding a host truncated at its own port
		// separator and "b/c" as the slug, an identity for a repo that does not
		// exist (Greptile, #98). The path is still reduced to its last two segments,
		// so a colon inside one rides along instead of re-cutting the URL.
		"git@host:a:b/c":                {"host", "a:b/c"},
		"git@github.com:owner/na:me":    {"github.com", "owner/na:me"},
		"git@host:group/repo:arch/name": {"host", "repo:arch/name"},

		// Every rejected form yields BOTH empty. There must be no state where a
		// host survives from a URL we refused to turn into an identity — a host
		// with no slug would be a fact about the machine attached to nothing.
		"":                             {"", ""},
		"not-a-url":                    {"", ""},
		"/home/alice/repos/myproject":  {"", ""},
		"./myproject":                  {"", ""},
		"file:///home/alice/myproject": {"", ""},
		"C:/Users/alice/repo":          {"", ""},
	}
	for in, want := range cases {
		host, slug := normalizeRemote(in)
		if host != want.host || slug != want.slug {
			t.Errorf("normalizeRemote(%q) = (%q, %q), want (%q, %q)", in, host, slug, want.host, want.slug)
		}
		// The wrapper must stay exactly the slug half — this is what makes the
		// split a no-op for every existing caller.
		if got := normalizeRemoteSlug(in); got != slug {
			t.Errorf("normalizeRemoteSlug(%q) = %q, but normalizeRemote returned slug %q", in, got, slug)
		}
	}
}

// A host is never reported without a slug. Stated as its own invariant because
// it is the property the redact allowlist depends on: repoHost is safe to ship
// only while it is a provider name attached to a real repo identity.
func TestNormalizeRemoteNeverHostWithoutSlug(t *testing.T) {
	inputs := []string{
		"", "not-a-url", "https://github.com", "https://github.com/",
		"https://github.com/onlyowner", "git@github.com:", "git@github.com:name",
		"file:///home/alice/myproject", "/home/alice/repo", "C:/Users/alice/repo",
		"ssh://git@github.com/name",
	}
	for _, in := range inputs {
		host, slug := normalizeRemote(in)
		if slug == "" && host != "" {
			t.Errorf("normalizeRemote(%q) = host %q with an empty slug — a host must never outlive the identity it came from", in, host)
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

// markGitRepo makes root look like the top of a git working tree so the
// nested-CLAUDE.md scan (which gates on .git presence) will descend into it.
func markGitRepo(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestProjectClaudeMdTokensSkipsNonRepoRoot is the v0.9.1 regression guard: the
// nested-CLAUDE.md fallback must NEVER descend into a root that is not a git
// repository. The autostart daemon's workspace falls back to the user's HOME
// dir (launchd WorkingDirectory=home, no PROMPTSTER_TEAMS_WATCH_DIR), and a
// 5-level WalkDir of home enumerates ~/Documents, ~/Downloads, ~/Music — every
// macOS TCC-protected folder — firing "wants to access your Downloads" consent
// prompts from a capture tool. A non-repo root contributes 0 and is never
// walked; the SAME tree marked as a repo is walked normally.
func TestProjectClaudeMdTokensSkipsNonRepoRoot(t *testing.T) {
	// A home-like directory: protected-folder-shaped subdirs, each with a
	// CLAUDE.md, but NO .git at the root.
	home := t.TempDir()
	writeClaudeFixture(t, home, "Documents/proj/CLAUDE.md", 400) // 100 tokens
	writeClaudeFixture(t, home, "Downloads/CLAUDE.md", 400)      // 100 tokens

	if got := projectClaudeMdTokens([]string{home}); got != 0 {
		t.Fatalf("non-repo root walked: got %d, want 0 (must not descend into a dir with no .git)", got)
	}

	// Same tree, now a git repo → the walk runs and finds the nested file.
	markGitRepo(t, home)
	if got := projectClaudeMdTokens([]string{home}); got != 100 {
		t.Fatalf("repo root: got %d, want 100 (max nested once .git present)", got)
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
	markGitRepo(t, inRoot)
	writeClaudeFixture(t, inRoot, "l1/l2/l3/l4/l5/CLAUDE.md", 200)
	if got := projectClaudeMdTokens([]string{inRoot}); got != 50 {
		t.Errorf("depth-5 file: got %d, want 50 (must be included)", got)
	}

	// Depth 6 (m1/.../m6/CLAUDE.md): dir m6 is 6 levels below root → excluded.
	outRoot := t.TempDir()
	markGitRepo(t, outRoot)
	writeClaudeFixture(t, outRoot, "m1/m2/m3/m4/m5/m6/CLAUDE.md", 200)
	if got := projectClaudeMdTokens([]string{outRoot}); got != 0 {
		t.Errorf("depth-6 file: got %d, want 0 (must be excluded)", got)
	}
}

// gitCmd runs git in dir, failing the test on error. Shared by the repo-discovery
// and workspace-key tests below.
func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

// writeTranscriptCwd writes a minimal Claude Code transcript under
// <CLAUDE_CONFIG_DIR>/projects/<slug>/session.jsonl whose first line records cwd
// — the shape discoverCensusRepoRoots reads to recover a repo.
func writeTranscriptCwd(t *testing.T, cfgDir, slug, cwd string) {
	t.Helper()
	p := filepath.Join(cfgDir, "projects", slug, "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"cwd":` + jsonString(cwd) + `,"timestamp":"2026-07-20T00:00:00Z"}` + "\n"
	if err := os.WriteFile(p, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestBuildConfigCensusEventsPerRepo is the core fix for the "CLAUDE.md coverage
// = 0%" bug: a device emits ONE config_census per real repo it captures for —
// discovered from transcript cwds, not the $HOME watch root — each carrying that
// repo's true workspaceKey (git slug when it has an origin remote, else the
// collapsed repo-root hash) and a non-zero projectClaudeMdTokens when the repo
// has a CLAUDE.md. The cwds point at SUBDIRS to prove canonicalization to root.
func TestBuildConfigCensusEventsPerRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home := t.TempDir()
	cfg := filepath.Join(home, ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)

	// repoA: origin remote → workspaceKey is the slug (matches outcome_events.repo).
	repoA := t.TempDir()
	gitCmd(t, repoA, "init")
	gitCmd(t, repoA, "remote", "add", "origin", "git@github.com:acme/repo-a.git")
	writeClaudeFixture(t, repoA, "CLAUDE.md", 400) // 100 tokens
	repoASub := filepath.Join(repoA, "src")
	if err := os.MkdirAll(repoASub, 0o755); err != nil {
		t.Fatal(err)
	}

	// repoB: no remote → workspaceKey is the collapsed repo-root hash.
	repoB := t.TempDir()
	gitCmd(t, repoB, "init")
	writeClaudeFixture(t, repoB, "CLAUDE.md", 80) // 20 tokens

	writeTranscriptCwd(t, cfg, "-repo-a-src", repoASub) // subdir cwd → must canonicalize to repoA root
	writeTranscriptCwd(t, cfg, "-repo-b", repoB)

	events := buildConfigCensusEvents(Session{DeviceID: "dev-fleet", TaskRoot: home})
	if len(events) != 2 {
		t.Fatalf("expected one census per discovered repo (2), got %d", len(events))
	}

	byKey := map[string]map[string]interface{}{}
	for _, ev := range events {
		data, ok := ev.Data.(map[string]interface{})
		if !ok {
			t.Fatalf("event Data is not a map: %T", ev.Data)
		}
		key, _ := data["workspaceKey"].(string)
		byKey[key] = data
		// Every emitted census must carry the full device inventory, not a husk.
		if _, ok := data["skills"]; !ok {
			t.Errorf("census for %q missing device inventory (skills)", key)
		}
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}

	// repoA → slug, projectClaudeMdTokens from its root CLAUDE.md.
	a, ok := byKey["acme/repo-a"]
	if !ok {
		t.Fatalf("no census keyed by repoA slug acme/repo-a; keys=%v", keys)
	}
	if tok, _ := a["projectClaudeMdTokens"].(float64); tok != 100 {
		t.Errorf("repoA projectClaudeMdTokens = %v, want 100", a["projectClaudeMdTokens"])
	}

	// repoB → opaque 16-hex repo-root hash (not a slug, not sha(HOME)).
	wantB := workspaceKey(gitRepoRoot(resolvePath(repoB)))
	b, ok := byKey[wantB]
	if !ok {
		t.Fatalf("no census keyed by repoB hash %q; keys=%v", wantB, keys)
	}
	if len(wantB) != 16 || strings.Contains(wantB, "/") {
		t.Errorf("repoB key must be a 16-hex non-slug hash, got %q", wantB)
	}
	if tok, _ := b["projectClaudeMdTokens"].(float64); tok != 20 {
		t.Errorf("repoB projectClaudeMdTokens = %v, want 20", b["projectClaudeMdTokens"])
	}
}

// TestBuildConfigCensusEventsNoActiveReposEmptyKey pins the fresh-device shape:
// with no transcript store (no repos to discover), the device emits exactly ONE
// census with an EMPTY workspaceKey — the honest "nothing to report yet" the
// backend coverage fold skips — NOT the old sha(HOME) pseudo-repo that pinned
// coverage to 0% fleet-wide.
func TestBuildConfigCensusEventsNoActiveReposEmptyKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude")) // no projects/ store

	events := buildConfigCensusEvents(Session{DeviceID: "dev-empty", TaskRoot: home})
	if len(events) != 1 {
		t.Fatalf("no active repos must yield exactly one device-only census, got %d", len(events))
	}
	data, ok := events[0].Data.(map[string]interface{})
	if !ok {
		t.Fatalf("event Data is not a map: %T", events[0].Data)
	}
	key, _ := data["workspaceKey"].(string)
	if key != "" {
		t.Errorf("no-active-repos census must carry an EMPTY workspaceKey, got %q", key)
	}
	// It must specifically NOT be the sha(HOME) pseudo-workspace of the old bug.
	if bogus := workspaceKey(home); bogus != "" && key == bogus {
		t.Errorf("census must not fall back to the sha(HOME) pseudo-workspace %q", bogus)
	}
}

// TestWorkspaceKeyCollapsesNoRemoteWorktrees pins Part A.2: for a git repo with
// NO origin remote, subdirs and linked worktrees must collapse to a single
// workspaceKey (the repo-root hash) — the raw-abspath fallback gave each a
// different key. A repo WITH a remote already collapses via its slug (covered by
// TestWorkspaceKeyPrefersGitRemoteSlug).
func TestWorkspaceKeyCollapsesNoRemoteWorktrees(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	main := t.TempDir()
	gitCmd(t, main, "init")
	gitCmd(t, main, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "--allow-empty", "-m", "init")

	// A subdir of the same no-remote repo → same key as the root.
	sub := filepath.Join(main, "pkg", "x")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := workspaceKey(sub), workspaceKey(main); got != want {
		t.Errorf("subdir key %q != root key %q (no-remote repo must collapse)", got, want)
	}

	// A linked worktree of the same no-remote repo → same key. This is the exact
	// case the abspath fallback got wrong.
	wt := filepath.Join(t.TempDir(), "wt")
	gitCmd(t, main, "worktree", "add", wt)
	if got, want := workspaceKey(wt), workspaceKey(main); got != want {
		t.Errorf("worktree key %q != root key %q (no-remote worktrees must collapse)", got, want)
	}

	// Still an opaque 16-hex hash, never a path.
	if k := workspaceKey(main); len(k) != 16 || strings.ContainsRune(k, filepath.Separator) {
		t.Errorf("no-remote workspaceKey must be a 16-hex hash, got %q", k)
	}
}
