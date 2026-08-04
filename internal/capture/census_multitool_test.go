package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The census now describes THREE agents, not one. These pin the two things that
// go wrong when it does: assets from different tools merging into one identity,
// and a config file's VALUES riding along with its section names.

func writeSkill(t *testing.T, dir, slug, name, desc string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, slug), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + desc + "\n---\n\nSECRET-SKILL-BODY\n"
	if err := os.WriteFile(filepath.Join(dir, slug, "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// multiToolEnv builds a machine with all three agents installed.
func multiToolEnv(t *testing.T) censusEnv {
	t.Helper()
	home := t.TempDir()
	claude := filepath.Join(home, ".claude")
	codex := filepath.Join(home, ".codex")
	cursor := filepath.Join(home, ".cursor")

	// The SAME slug under two tools — the collision this change exists to keep apart.
	writeSkill(t, filepath.Join(claude, "skills"), "review", "review", "claude's review")
	writeSkill(t, filepath.Join(codex, "skills"), "review", "review", "codex's review")
	writeSkill(t, filepath.Join(codex, "skills"), "refactor", "refactor", "codex only")
	// Codex's bundled set sits one level deeper.
	writeSkill(t, filepath.Join(codex, "skills", ".system"), "openai-docs", "openai-docs", "bundled")
	writeSkill(t, filepath.Join(cursor, "skills-cursor"), "babysit", "babysit", "cursor only")

	// Codex MCP: TOML. The section NAMES may ship; nothing else may.
	codexTOML := `approvals_reviewer = "auto_review"

[projects."/Users/x/repos/thing"]
trust_level = "trusted"

[mcp_servers.probeserver]
command = "npx"
args = ["-y", "SECRET-MCP-PACKAGE"]

[mcp_servers.probeserver.env]
TOKEN = "SECRET-MCP-ENV"

[mcp_servers."spaced name"]
command = "node"
url = "https://SECRET-MCP-URL"
`
	if err := os.WriteFile(filepath.Join(codex, "config.toml"), []byte(codexTOML), 0o600); err != nil {
		t.Fatal(err)
	}

	// Cursor MCP: the same `mcpServers` JSON object shape as .mcp.json.
	cursorMCP := `{"mcpServers":{"supabase":{"command":"npx","env":{"KEY":"SECRET-CURSOR-ENV"}},"clerk":{"url":"https://SECRET-CURSOR-URL"}}}`
	if err := os.WriteFile(filepath.Join(cursor, "mcp.json"), []byte(cursorMCP), 0o600); err != nil {
		t.Fatal(err)
	}

	// Claude Code's own MCP config, so the Cursor prefix has something it could
	// wrongly spread to. Without this the no-leak assertion is vacuous.
	claudeJSON := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(claudeJSON, []byte(`{"mcpServers":{"github":{"command":"npx"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	return censusEnv{
		claudeDir:      claude,
		claudeJSONPath: claudeJSON,
		workspaceRoots: []string{t.TempDir()},
		codexDir:       codex,
		cursorDir:      cursor,
	}
}

func TestCensusKeepsSameSlugOnTwoToolsApart(t *testing.T) {
	data := buildConfigCensus(multiToolEnv(t))

	seen := map[string]int{}
	for _, s := range data.Skills {
		seen[s.Tool+"/"+s.Slug]++
	}
	for _, want := range []string{
		"claude-code/review",
		"codex/review",
		"codex/refactor",
		"codex/openai-docs", // bundled, one level deeper
		"cursor/babysit",
	} {
		if seen[want] != 1 {
			t.Errorf("want exactly one %s, got %d (all: %v)", want, seen[want], seen)
		}
	}
	// The collision case, stated as its own assertion: two entries, not one.
	// Folding them hands one asset's carry to the other's invocations.
	if seen["claude-code/review"]+seen["codex/review"] != 2 {
		t.Errorf("the two `review` skills folded together: %v", seen)
	}
	if data.SkillCount != len(data.Skills) {
		t.Errorf("skillCount %d disagrees with skills %d", data.SkillCount, len(data.Skills))
	}
}

func TestCensusReadsMCPFromAllThreeTools(t *testing.T) {
	data := buildConfigCensus(multiToolEnv(t))

	got := map[string]string{}
	for _, m := range data.MCPServers {
		got[m.Name] = m.Tool
	}
	for name, wantTool := range map[string]string{
		"probeserver": toolCodex,
		"spaced name": toolCodex, // quoted TOML key, unquoted
		// Cursor's, under the name CURSOR ITSELF USES on the wire — see below.
		"user-supabase": toolCursor,
		"user-clerk":    toolCursor,
		"github":        toolClaudeCode,
	} {
		if got[name] != wantTool {
			t.Errorf("MCP %q tool = %q, want %q (all: %v)", name, got[name], wantTool, got)
		}
	}
	// A dotted sub-table under a server is the SAME server, not a second one
	// called "probeserver.env".
	if _, bad := got["probeserver.env"]; bad {
		t.Errorf("a TOML sub-table was minted as its own MCP server: %v", got)
	}

	// A Cursor server censused under its BARE mcp.json key can never join its
	// own invocations: across 89 real transcripts Cursor names user-scope
	// servers `user-<key>` on the wire. Censusing `supabase` while the
	// invocations say `user-supabase` matches zero rows — silently, and looking
	// exactly like "this engineer never used their MCP servers".
	for _, bare := range []string{"supabase", "clerk"} {
		if _, bad := got[bare]; bad {
			t.Errorf("cursor MCP %q censused under its bare config key; the wire name is %q, so this joins nothing: %v",
				bare, cursorUserMCPPrefix+bare, got)
		}
	}
	// Claude's own servers must NOT pick the prefix up.
	if got["user-github"] != "" {
		t.Errorf("the cursor prefix leaked onto a non-cursor server: %v", got)
	}
}

// The one that matters. A config file is mostly secrets; only its section names
// may ship. This asserts on the SERIALIZED payload, because a field that never
// reaches the struct cannot leak but a field that does can.
func TestCensusEmitsNoConfigValues(t *testing.T) {
	data := buildConfigCensus(multiToolEnv(t))
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	payload := string(raw)

	for _, leak := range []string{
		"SECRET-MCP-ENV",     // Codex TOML env value
		"SECRET-MCP-PACKAGE", // Codex TOML args value
		"SECRET-MCP-URL",     // Codex TOML url
		"SECRET-CURSOR-ENV",  // Cursor mcp.json env value
		"SECRET-CURSOR-URL",  // Cursor mcp.json url
		"SECRET-SKILL-BODY",  // any skill body, any tool
		"npx", "node",        // MCP launch commands
		"trust_level",          // any other TOML key
		"/Users/x/repos/thing", // a path from an unrelated TOML section
		"config.toml",          // no config path may ship
		".codex", ".cursor",    // no config root path may ship
	} {
		if strings.Contains(payload, leak) {
			t.Errorf("census leaks %q: %s", leak, payload)
		}
	}
}

// Examined-and-empty must be distinguishable from never-looked-at. An empty
// skills list cannot say which, and reading the first as the second is how a
// tool gets declared unused on the strength of never having been measured.
func TestCensusRecordsWhichToolsItExamined(t *testing.T) {
	full := buildConfigCensus(multiToolEnv(t))
	if len(full.ToolsExamined) != 3 {
		t.Fatalf("want all three roots examined, got %v", full.ToolsExamined)
	}

	// Codex installed but holding no skills at all.
	home := t.TempDir()
	codex := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codex, 0o755); err != nil {
		t.Fatal(err)
	}
	empty := buildConfigCensus(censusEnv{
		claudeDir:      filepath.Join(home, ".claude"),
		claudeJSONPath: filepath.Join(home, ".claude.json"),
		codexDir:       codex,
	})
	// Not installed at all.
	absent := buildConfigCensus(censusEnv{
		claudeDir:      filepath.Join(home, ".claude"),
		claudeJSONPath: filepath.Join(home, ".claude.json"),
		codexDir:       filepath.Join(home, "no-such-dir"),
	})

	if len(empty.Skills) != 0 || len(absent.Skills) != 0 {
		t.Fatalf("setup: both should report zero skills")
	}
	hasCodex := func(d configCensusData) bool {
		for _, tool := range d.ToolsExamined {
			if tool == toolCodex {
				return true
			}
		}
		return false
	}
	if !hasCodex(empty) {
		t.Errorf("an installed-but-empty Codex was not recorded as examined: %v", empty.ToolsExamined)
	}
	if hasCodex(absent) {
		t.Errorf("an absent Codex was recorded as examined: %v", absent.ToolsExamined)
	}
}

// A machine with no Claude Code at all must census cleanly rather than erroring
// or reporting Claude assets it does not have. This is the affected customer's
// actual shape.
func TestCensusOnACodexOnlyMachine(t *testing.T) {
	home := t.TempDir()
	codex := filepath.Join(home, ".codex")
	writeSkill(t, filepath.Join(codex, "skills"), "refactor", "refactor", "codex only")

	data := buildConfigCensus(censusEnv{
		claudeDir:      filepath.Join(home, ".claude"), // does not exist
		claudeJSONPath: filepath.Join(home, ".claude.json"),
		codexDir:       codex,
	})

	if len(data.Skills) != 1 || data.Skills[0].Slug != "refactor" || data.Skills[0].Tool != toolCodex {
		t.Fatalf("want one codex skill, got %+v", data.Skills)
	}
	if len(data.Plugins) != 0 {
		t.Errorf("want no plugins on a machine with no Claude Code, got %+v", data.Plugins)
	}
	if data.GlobalClaudeMdTokens != 0 {
		t.Errorf("want zero Claude memory tokens, got %d", data.GlobalClaudeMdTokens)
	}
}

// A census taken with only the Claude root configured must be byte-identical to
// the pre-change one except for the tool stamps and toolsExamined. This is the
// regression that protects every stored census and every existing board.
func TestCensusUnchangedForAClaudeOnlyMachine(t *testing.T) {
	home := t.TempDir()
	claude := filepath.Join(home, ".claude")
	writeSkill(t, filepath.Join(claude, "skills"), "deploy-check", "deploy-check", "d")

	data := buildConfigCensus(censusEnv{
		claudeDir:      claude,
		claudeJSONPath: filepath.Join(home, ".claude.json"),
		// codexDir/cursorDir empty: the tool walks are skipped entirely.
	})

	if len(data.Skills) != 1 || data.Skills[0].Tool != toolClaudeCode {
		t.Fatalf("want one claude-code skill, got %+v", data.Skills)
	}
	if len(data.ToolsExamined) != 1 || data.ToolsExamined[0] != toolClaudeCode {
		t.Errorf("toolsExamined = %v, want just claude-code", data.ToolsExamined)
	}
}

// A TOML table header may carry a trailing comment, and a comment is arbitrary
// local text an engineer wrote for themselves. It must not become a server NAME
// and ride out on the census — `mcpServers[].name` is allowlisted, so the
// projection cannot catch this. The parse is the only place it can be stopped.
func TestCodexMCPHeaderCommentsNeverBecomeNames(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	body := "" +
		"[mcp_servers.db] # staging creds live in 1password/eng-shared\n" +
		"command = \"docker\"\n" +
		"[mcp_servers.\"my server\"]   # internal only\n" +
		"[mcp_servers.linear.env] # do not share\n" +
		"[not_mcp.thing] # ignored entirely\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	got := censusCodexMCPServers(cfg)
	names := make([]string, 0, len(got))
	for _, s := range got {
		names = append(names, s.Name)
		if strings.ContainsAny(s.Name, "#]") {
			t.Fatalf("comment or bracket leaked into a server name: %q", s.Name)
		}
		if strings.Contains(strings.ToLower(s.Name), "1password") ||
			strings.Contains(strings.ToLower(s.Name), "do not share") {
			t.Fatalf("local comment text egressed in a name: %q", s.Name)
		}
	}
	want := []string{"db", "linear", "my server"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

// A server name may legitimately contain `]` inside quotes. Bounding the header
// at the FIRST bracket would truncate it, so the scan tracks quote state.
func TestCodexMCPQuotedNameKeepsBracket(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfg, []byte("[mcp_servers.\"a]b\"] # x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := censusCodexMCPServers(cfg)
	if len(got) != 1 || got[0].Name != "a]b" {
		t.Fatalf("got %#v, want one server named %q", got, "a]b")
	}
}

// The WATCHERS honor CODEX_HOME and PROMPTSTER_CURSOR_HOME. A census that reads
// a hardcoded ~/.codex inventories one machine's assets while the watcher
// captures another's sessions — and reports `toolsExamined` for a directory
// nobody is working in.
func TestCensusEnvHonorsVendorHomeOverrides(t *testing.T) {
	codex := t.TempDir()
	cursor := t.TempDir()
	t.Setenv("CODEX_HOME", codex)
	t.Setenv("PROMPTSTER_CURSOR_HOME", cursor)

	env := defaultCensusEnv("")
	if env.codexDir != codex {
		t.Fatalf("codexDir = %q, want CODEX_HOME %q", env.codexDir, codex)
	}
	if env.cursorDir != cursor {
		t.Fatalf("cursorDir = %q, want PROMPTSTER_CURSOR_HOME %q", env.cursorDir, cursor)
	}
}
