package normalize

import "testing"

func TestSkillNameFromPath(t *testing.T) {
	tests := map[string]string{
		"/Users/u/.agents/skills/audit/SKILL.md":                 "audit",
		"/Users/u/.codex/skills/.system/openai-docs/SKILL.md":    "openai-docs",
		"C:\\Users\\u\\.cursor\\skills-cursor\\review\\SKILL.md": "review",
		"/repo/skills/audit/reference.md":                        "",
	}
	for path, want := range tests {
		if got := skillNameFromPath(path); got != want {
			t.Errorf("skillNameFromPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestSkillNameFromCommand(t *testing.T) {
	tests := map[string]string{
		"sed -n '1,240p' /Users/u/.codex/skills/.system/openai-docs/SKILL.md": "openai-docs",
		"cat /Users/u/.agents/skills/audit/SKILL.md":                          "audit",
		"rg -n SKILL.md /Users/u/.agents/skills":                              "",
		"find /Users/u/.agents/skills -name SKILL.md":                         "",
	}
	for command, want := range tests {
		if got := skillNameFromCommand(command); got != want {
			t.Errorf("skillNameFromCommand(%q) = %q, want %q", command, got, want)
		}
	}
}
