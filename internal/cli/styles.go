package cli

import "github.com/charmbracelet/lipgloss"

// Adaptive text colors. The CLI was originally styled for dark terminals, so
// every grayscale foreground was a near-white (#ffffff/#d4d4d4/#e5e7eb) that
// vanishes on a light background (Terminal.app's default is white). lipgloss
// auto-detects the terminal background and picks the matching side, so these
// stay legible in both. Brand accents (green/amber/red/sky) are left as fixed
// colors — they read acceptably on either background and are intentional.
var (
	// cStrong: titles and strong emphasis (was #ffffff).
	cStrong = lipgloss.AdaptiveColor{Light: "#18181b", Dark: "#ffffff"}
	// cBody: primary body text (was #d4d4d4 / #e5e7eb / #e0e0e0).
	cBody = lipgloss.AdaptiveColor{Light: "#3f3f46", Dark: "#d4d4d4"}
	// cMuted: secondary / meta text (was #a3a3a3 / #9ca3af).
	cMuted = lipgloss.AdaptiveColor{Light: "#52525b", Dark: "#a3a3a3"}
	// cDim: lowest-emphasis dim text (was #888888 / #888).
	cDim = lipgloss.AdaptiveColor{Light: "#71717a", Dark: "#888888"}
	// cWarnText: warning body text (was #fcd34d, invisible on white).
	cWarnText = lipgloss.AdaptiveColor{Light: "#b45309", Dark: "#fcd34d"}
	// cGold: gold warning emphasis used as text (was #eab308).
	cGold = lipgloss.AdaptiveColor{Light: "#a16207", Dark: "#eab308"}
)
