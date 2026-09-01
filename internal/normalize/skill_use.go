package normalize

import (
	"path/filepath"
	"regexp"
	"strings"
)

// skillNameFromPath recognizes a direct read of a skill's instruction file.
// The optional container segment covers Codex's bundled
// skills/.system/<name>/SKILL.md layout without treating .system as the skill.
var skillPathPattern = regexp.MustCompile(`(?i)(?:^|[/\\])skills(?:-[a-z0-9]+)?(?:[/\\]\.[a-z0-9._-]+)?[/\\]([a-z0-9][a-z0-9._-]{0,63})[/\\]skill\.md$`)

func skillNameFromPath(path string) string {
	clean := filepath.ToSlash(strings.TrimSpace(path))
	m := skillPathPattern.FindStringSubmatch(clean)
	if len(m) != 2 {
		return ""
	}
	return strings.ToLower(m[1])
}

// Restrict shell inference to commands whose purpose is reading a file. Search
// commands can mention dozens of SKILL.md paths without activating any of them.
var directReadCommand = regexp.MustCompile(`(?i)(?:^|[;&|]\s*)(?:cat|bat|head|tail|less|more|sed\b[^;&|]*|awk\b[^;&|]*)\s+[^;&|]*?((?:[^\s"']+[/\\])skills(?:-[a-z0-9]+)?(?:[/\\]\.[a-z0-9._-]+)?[/\\][a-z0-9][a-z0-9._-]{0,63}[/\\]SKILL\.md)(?:\s|$)`)

func skillNameFromCommand(command string) string {
	m := directReadCommand.FindStringSubmatch(command)
	if len(m) != 2 {
		return ""
	}
	return skillNameFromPath(m[1])
}
