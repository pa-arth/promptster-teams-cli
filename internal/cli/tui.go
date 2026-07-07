package cli

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Setup-TUI styling. Mirrors the promptster-cli look: a green block-bar brand,
// a green "❯" prompt, rounded boxed panels, and ✓/!/✗ status glyphs — all on
// top of the adaptive light/dark palette in styles.go. Lipgloss auto-detects a
// non-TTY (pipe/CI) and emits plain text, so output stays clean when redirected.

// cAccent is the brand green used for the bar, prompt, and success glyph. Fixed
// (not adaptive) — it reads acceptably on both light and dark, matching the
// hiring CLI's accent.
var cAccent = lipgloss.Color("#22c55e")

var (
	brandStyle  = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	titleStyle  = lipgloss.NewStyle().Foreground(cStrong).Bold(true)
	bodyStyle   = lipgloss.NewStyle().Foreground(cBody)
	mutedStyle  = lipgloss.NewStyle().Foreground(cMuted)
	dimStyle    = lipgloss.NewStyle().Foreground(cDim)
	promptStyle = lipgloss.NewStyle().Foreground(cAccent).Bold(true)

	okGlyph   = lipgloss.NewStyle().Foreground(cAccent).Bold(true).Render("✓")
	warnGlyph = lipgloss.NewStyle().Foreground(cGold).Bold(true).Render("!")
	errGlyph  = lipgloss.NewStyle().Foreground(lipgloss.Color("#ef4444")).Bold(true).Render("✗")

	panelBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(cDim).
			Padding(0, 2)

	panelLabel = lipgloss.NewStyle().Foreground(cMuted).Width(9)
	panelValue = lipgloss.NewStyle().Foreground(cBody)
)

// brandBar renders the "▍promptster-teams · <suffix>" header.
func brandBar(suffix string) string {
	bar := brandStyle.Render("▍promptster-teams")
	if suffix == "" {
		return "  " + bar
	}
	return "  " + bar + mutedStyle.Render(" · "+suffix)
}

// promptGlyph is the green "❯" used before interactive prompts.
func promptGlyph() string { return promptStyle.Render("❯") }

// kvPanel renders a rounded box titled with the given header and one
// label/value row per pair (pairs are [label, value, label, value, ...]).
func kvPanel(title string, pairs ...string) string {
	var rows []string
	rows = append(rows, titleStyle.Render(title), "")
	for i := 0; i+1 < len(pairs); i += 2 {
		rows = append(rows, panelLabel.Render(pairs[i])+panelValue.Render(pairs[i+1]))
	}
	return panelBox.Render(strings.Join(rows, "\n"))
}

// indent prefixes every line of s with two spaces so boxed/multiline content
// lines up with the rest of the setup output.
func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

// printlnIndent prints a single two-space-indented line.
func printlnIndent(s string) { fmt.Println("  " + s) }
