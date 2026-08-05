package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The personal roots (~/.claude/skills and friends) are the MINORITY of what is
// installed. On the machine this was written against they held 37 skills against
// ~81 elsewhere, and on the one Codex-heavy customer engineer measured, every
// skill he actually runs lives in a plugin cache and NONE of his censused ones
// was ever activated. These pin the roots that were missing and the three ways
// walking them goes wrong.

// pluginSkill writes <cache>/<marketplace>/<plugin>/<version>/skills/<slug>/SKILL.md.
func pluginSkill(t *testing.T, cache, marketplace, plugin, version, slug string) {
	t.Helper()
	writeSkill(t, filepath.Join(cache, marketplace, plugin, version, "skills"), slug, slug, "desc of "+slug)
}

func skillRootsEnv(t *testing.T) (censusEnv, string) {
	t.Helper()
	home := t.TempDir()
	claude := filepath.Join(home, ".claude")
	codex := filepath.Join(home, ".codex")

	writeSkill(t, filepath.Join(claude, "skills"), "personal", "personal", "a personal skill")
	writeSkill(t, filepath.Join(home, ".agents", "skills"), "shared-one", "shared-one", "cross-client")

	// Codex plugin cache: two versions of ONE plugin, plus a directory that is
	// not a skill at all.
	codexCache := filepath.Join(codex, "plugins", "cache")
	pluginSkill(t, codexCache, "openai-curated-remote", "superpowers", "6.1.1", "tdd")
	pluginSkill(t, codexCache, "openai-curated-remote", "superpowers", "6.2.0", "tdd")
	pluginSkill(t, codexCache, "openai-curated-remote", "superpowers", "6.2.0", "brainstorming")
	// `retired` shipped in 6.1.1 and was REMOVED in 6.2.0. It is the only thing
	// separating "read the newest version" from "read all of them": a duplicate
	// slug is folded by dedupeSkills either way, so asserting on tdd's count
	// tests dedupe and not the version fold. It did exactly that, and passed
	// with the fold reverted.
	pluginSkill(t, codexCache, "openai-curated-remote", "superpowers", "6.1.1", "retired")
	if err := os.MkdirAll(filepath.Join(codexCache, "openai-curated-remote", "superpowers", "6.2.0", "assets", "not-a-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 6.2.0 must be the newest by MTIME — version STRINGS are not comparable in
	// a real cache (semver, bare SHAs and the literal "unknown" all occur).
	bump(t, filepath.Join(codexCache, "openai-curated-remote", "superpowers", "6.2.0"))

	return censusEnv{
		claudeDir:      claude,
		claudeJSONPath: filepath.Join(home, ".claude.json"),
		codexDir:       codex,
		homeDir:        home,
	}, home
}

// bump makes dir the most recently modified of its siblings.
func bump(t *testing.T, dir string) {
	t.Helper()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	later := info.ModTime().Add(time.Hour)
	if err := os.Chtimes(dir, later, later); err != nil {
		t.Fatal(err)
	}
}

func skillsBySlug(data configCensusData) map[string]censusSkill {
	out := map[string]censusSkill{}
	for _, s := range data.Skills {
		out[s.Tool+"/"+s.Slug] = s
	}
	return out
}

func TestCensusFindsThePluginCacheAndTheSharedRoot(t *testing.T) {
	env, _ := skillRootsEnv(t)
	got := skillsBySlug(buildConfigCensus(env))

	for _, want := range []struct{ key, source, plugin string }{
		{"claude-code/personal", skillSourceUser, ""},
		{"/shared-one", skillSourceAgents, ""}, // no tool: the root belongs to none
		{"codex/tdd", skillSourcePluginCache, "superpowers"},
		{"codex/brainstorming", skillSourcePluginCache, "superpowers"},
	} {
		s, ok := got[want.key]
		if !ok {
			t.Errorf("missing %s (have %v)", want.key, skillKeys(got))
			continue
		}
		if s.Source != want.source {
			t.Errorf("%s source = %q, want %q", want.key, s.Source, want.source)
		}
		if s.Plugin != want.plugin {
			t.Errorf("%s plugin = %q, want %q", want.key, s.Plugin, want.plugin)
		}
	}
	// A directory with no SKILL.md is not an asset. This is the ORIGINAL and
	// correct objection to walking a download cache — answered, not inherited.
	if _, ok := got["codex/not-a-skill"]; ok {
		t.Error("inventoried a directory with no SKILL.md; a fabricated asset is worse than a missing one")
	}
}

// The cache KEEPS old versions — superpowers at 6.1.1 and 6.2.0 at once, one
// plugin at four versions, another at both a SHA and the literal "unknown". A
// naive walk inventories one skill twice and doubles its carry, manufacturing
// dead weight out of a stale download.
func TestCensusCountsOnePluginSkillOncePerVersionSet(t *testing.T) {
	env, _ := skillRootsEnv(t)
	data := buildConfigCensus(env)

	n := 0
	for _, s := range data.Skills {
		if s.Slug == "tdd" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("tdd inventoried %d times across two cached versions, want 1", n)
	}
	// The load-bearing half: a skill the installed version no longer ships is
	// not carried. Reading every cached version resurrects it, and a phantom
	// asset lands on the dead-weight board with nothing to delete.
	for _, s := range data.Skills {
		if s.Slug == "retired" {
			t.Errorf("inventoried %q from a superseded cached version", s.Slug)
		}
	}
	// And the carry follows the count — a duplicate would double-bill silently.
	total := 0
	for _, s := range data.Skills {
		total += s.DescTokens
	}
	if total != data.SkillListingTokens {
		t.Errorf("listing tokens %d disagree with the summed rows %d", data.SkillListingTokens, total)
	}
}

// Linking a shared skill into a client's own root is a documented workflow —
// Gemini CLI ships `/skills link` for it, and 4 of 17 skills under
// ~/.claude/skills on the machine this was written against are symlinks into
// ~/.agents/skills. os.ReadDir does not follow symlinks, so every one of them
// was reported as ABSENT: an asset the engineer has and uses, counted as zero.
func TestCensusFollowsASymlinkedSkillDirectory(t *testing.T) {
	env, home := skillRootsEnv(t)
	writeSkill(t, filepath.Join(home, ".agents", "skills"), "linked", "linked", "lives in the shared root")
	if err := os.Symlink(
		filepath.Join(home, ".agents", "skills", "linked"),
		filepath.Join(home, ".claude", "skills", "linked"),
	); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	data := buildConfigCensus(env)
	got := skillsBySlug(data)
	if _, ok := got["claude-code/linked"]; !ok {
		t.Fatalf("symlinked skill missing from the Claude root (have %v)", skillKeys(got))
	}

	// ...and exactly once overall. It passes the (tool, slug) key twice — once as
	// claude-code, once as the tool-less shared entry — so only resolving the
	// path catches it, and without that one physical skill bills twice.
	n := 0
	for _, s := range data.Skills {
		if s.Slug == "linked" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("one physical skill reached by two routes counted %d times, want 1", n)
	}
}

// Claude's registry resolves an installPath that lives INSIDE plugins/cache, so
// the registry walk and the cache walk overlap by construction. The enabled
// route must win, because it is the one that knows the skill is actually loaded.
func TestCensusPrefersTheConfirmedEnabledPluginOverABareCacheHit(t *testing.T) {
	env, home := skillRootsEnv(t)
	claude := filepath.Join(home, ".claude")
	cache := filepath.Join(claude, "plugins", "cache")
	pluginSkill(t, cache, "official", "frontend-design", "1.0.0", "canvas")

	installPath := filepath.Join(cache, "official", "frontend-design", "1.0.0")
	registry := `{"version":1,"plugins":{"frontend-design@official":[{"installPath":"` + installPath + `"}]}}`
	if err := os.MkdirAll(filepath.Join(claude, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claude, "plugins", "installed_plugins.json"), []byte(registry), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claude, "settings.json"),
		[]byte(`{"enabledPlugins":{"frontend-design@official":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got := skillsBySlug(buildConfigCensus(env))
	s, ok := got["claude-code/canvas"]
	if !ok {
		t.Fatalf("plugin skill missing (have %v)", skillKeys(got))
	}
	if s.Source != skillSourcePlugin {
		t.Errorf("source = %q, want %q — enablement is KNOWABLE for Claude and the census must say so", s.Source, skillSourcePlugin)
	}
	if s.Plugin != "frontend-design" {
		t.Errorf("plugin = %q, want the plugin half of the registry key", s.Plugin)
	}
}

// toolsExamined is the census's own instance of "an instrument that never ran
// reports the same zero as a real zero". A new root needs it as much as the
// tools did — more, since it is the one holding a third of the assets.
func TestCensusRecordsTheSharedRootAsExamined(t *testing.T) {
	env, _ := skillRootsEnv(t)
	examined := strings.Join(buildConfigCensus(env).ToolsExamined, ",")
	if !strings.Contains(examined, sharedSkillsRoot) {
		t.Errorf("toolsExamined = %q, missing %q", examined, sharedSkillsRoot)
	}

	// Absent root ⇒ NOT examined. Without this the field says "we looked" for a
	// directory that does not exist, which is the exact confusion it prevents.
	env.homeDir = t.TempDir()
	examined = strings.Join(buildConfigCensus(env).ToolsExamined, ",")
	if strings.Contains(examined, sharedSkillsRoot) {
		t.Errorf("toolsExamined = %q claims a root that is not there", examined)
	}
}

// The payload is names and counts. absPath exists only to recognise one skill
// reached twice, and it is unexported so encoding/json CANNOT emit it — this
// asserts the property rather than trusting the lowercase letter.
func TestCensusNeverSerializesASkillPath(t *testing.T) {
	env, home := skillRootsEnv(t)
	raw, err := json.Marshal(buildConfigCensus(env))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), home) {
		t.Errorf("census payload carries a filesystem path:\n%s", raw)
	}
	for _, banned := range []string{"absPath", "SECRET-SKILL-BODY", "plugins/cache"} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("census payload carries %q:\n%s", banned, raw)
		}
	}
}

func skillKeys(m map[string]censusSkill) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
