package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/pa-arth/promptster-teams-cli/internal/capture"
	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
	"github.com/pa-arth/promptster-teams-cli/internal/service"
)

// Live-status dashboard. A small bubbletea program that re-reads the capture
// snapshot every second and re-renders the panels in place, so `status` becomes
// a live view of the running daemon instead of a one-shot print. It falls back
// to the static print when stdout is not a TTY (pipe/CI) or `--once` is passed.

const staleHeartbeat = 30 * time.Second

var (
	dotOK   = lipgloss.NewStyle().Foreground(cAccent).Bold(true) // ● green
	dotWarn = lipgloss.NewStyle().Foreground(cGold).Bold(true)   // ● gold
	dotIdle = lipgloss.NewStyle().Foreground(cDim).Bold(true)    // ○ dim
)

type statusTickMsg time.Time

func statusTick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return statusTickMsg(t) })
}

type statusModel struct {
	snap      capture.CaptureSnapshot
	buffered  int
	token     string
	source    string
	apiURL    string
	device    string
	autostart string
	now       time.Time
	tick      int
	quitting  bool
}

func newStatusModel() statusModel {
	token, source := ingest.ResolveToken("")
	m := statusModel{
		token:  token,
		source: source,
		apiURL: ingest.ResolveAPIURL(""),
		device: capture.DeviceID(),
		now:    time.Now(),
	}
	m.snap = capture.Snapshot()
	m.buffered = countBufferedEvents()
	m.autostart = autostartLine()
	return m
}

// autostartLine probes the OS service manager (launchctl/systemctl/schtasks) and
// renders a colored status line. It is deliberately NOT called from the render
// path: the probe spawns a subprocess, so it runs once at start and only on an
// explicit refresh, never on every tick or keypress. Green is reserved for a
// service that is actually active — an installed-but-inactive service (e.g.
// "enabled (systemd --user, inactive)" or macOS "installed but not loaded")
// gets a warn dot so a broken autostart never reads as healthy.
func autostartLine() string {
	installed, detail, err := service.New().Status()
	if err != nil || !installed || detail == "" {
		return dotWarn.Render("○") + dimStyle.Render(" off — ") + bodyStyle.Render("promptster-teams autostart enable")
	}
	healthy := strings.HasPrefix(detail, "enabled") &&
		!strings.Contains(detail, "inactive") &&
		!strings.Contains(detail, "failed")
	if healthy {
		return dotOK.Render("●") + " " + detail
	}
	return dotWarn.Render("●") + " " + detail + dimStyle.Render(" — re-run ") + bodyStyle.Render("autostart enable")
}

func (m statusModel) Init() tea.Cmd { return statusTick() }

func (m statusModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "r":
			m.snap = capture.Snapshot()
			m.buffered = countBufferedEvents()
			m.autostart = autostartLine()
			return m, nil
		}
	case statusTickMsg:
		m.now = time.Time(msg)
		m.tick++
		m.snap = capture.Snapshot()
		m.buffered = countBufferedEvents()
		return m, statusTick()
	}
	return m, nil
}

func (m statusModel) View() string {
	if m.quitting {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(brandBar("status"))
	b.WriteString("\n\n")
	b.WriteString(indent(m.capturePanel()))
	b.WriteString("\n\n")
	b.WriteString(indent(m.watchersPanel()))
	b.WriteString("\n\n")
	b.WriteString(indent(m.bufferPanel()))
	b.WriteString("\n\n")
	b.WriteString(m.footer())
	b.WriteString("\n")
	return b.String()
}

func (m statusModel) capturePanel() string {
	var state string
	if m.snap.Live {
		up := ""
		if s := m.snap.StartedAt(); !s.IsZero() {
			up = ", up " + humanizeDuration(m.now.Sub(s))
		}
		state = dotOK.Render("●") + fmt.Sprintf(" active  (pid %d%s)", m.snap.DaemonPID, up)
	} else {
		state = dotIdle.Render("○") + dimStyle.Render(" idle — run ") + bodyStyle.Render("promptster-teams start")
	}
	// autostart is probed off the render path (see autostartLine) and cached on
	// the model — surface it live so an installed-but-idle seat is visible.
	return kvPanel("capture",
		"state", state,
		"autostart", m.autostart,
		"ingest", hostOf(m.apiURL),
		"key", keyDisplay(m.token, m.source),
		"device", m.device,
	)
}

func (m statusModel) watchersPanel() string {
	return kvPanel("watchers",
		"claude", watcherLine(m.snap.Claude, m.now, true),
		"codex", watcherLine(m.snap.Codex, m.now, false),
	)
}

// watcherLine renders one watcher's status: a colored health dot, event/byte
// counters, and heartbeat freshness. showBytes is off for codex (its pidfile
// carries no byte counter).
func watcherLine(w capture.WatcherStat, now time.Time, showBytes bool) string {
	if !w.Running {
		return dotIdle.Render("○") + dimStyle.Render(" not running")
	}
	var dot, label string
	switch {
	case w.Degraded:
		dot, label = dotWarn.Render("●"), "degraded"
	default:
		dot, label = dotOK.Render("●"), "healthy"
	}
	parts := []string{fmt.Sprintf("%d events", w.EventsCaptured)}
	if showBytes && w.BytesConsumed > 0 {
		parts = append(parts, humanizeBytes(w.BytesConsumed))
	}
	if !w.LastHeartbeat.IsZero() {
		age := now.Sub(w.LastHeartbeat)
		switch {
		case age < 0:
			// Heartbeat in the future — clock skew or a malformed pidfile. Don't
			// let humanizeDuration clamp it to "0s" and read as fresh.
			parts = append(parts, dotWarn.Render("♥ clock skew"))
		case age > staleHeartbeat:
			parts = append(parts, dotWarn.Render("♥ "+humanizeDuration(age)+" ago"))
		default:
			parts = append(parts, dimStyle.Render("♥ "+humanizeDuration(age)+" ago"))
		}
	}
	return dot + " " + label + dimStyle.Render(" · ") + strings.Join(parts, dimStyle.Render(" · "))
}

func (m statusModel) bufferPanel() string {
	pending := fmt.Sprintf("%d events pending upload", m.buffered)
	if m.buffered == 0 {
		pending = dotOK.Render("●") + " all events shipped"
	} else {
		pending = dotWarn.Render("●") + " " + pending
	}
	return kvPanel("buffer", "local", pending)
}

func (m statusModel) footer() string {
	pulse := dotOK.Render("●")
	if m.tick%2 == 1 {
		pulse = dotIdle.Render("●")
	}
	hints := dimStyle.Render("q quit · r refresh")
	clock := dimStyle.Render(pulse + " live · " + m.now.Format("15:04:05"))
	return "  " + hints + dimStyle.Render("   ·   ") + clock
}

// runStatusTUI launches the live dashboard. Input is taken from /dev/tty when
// available so it still works if stdin was redirected. Returns an error only if
// the program itself fails to run — callers fall back to the static print.
func runStatusTUI() error {
	opts := []tea.ProgramOption{tea.WithAltScreen()}
	if tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
		opts = append(opts, tea.WithInput(tty))
		defer tty.Close()
	}
	_, err := tea.NewProgram(newStatusModel(), opts...).Run()
	return err
}

// stdoutIsTTY reports whether stdout is an interactive terminal, so `status`
// only opens the full-screen dashboard when a human is watching.
func stdoutIsTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

// humanizeDuration renders a compact "1h38m" / "2m" / "45s" string.
func humanizeDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		h := int(d.Hours())
		mm := int(d.Minutes()) - h*60
		return fmt.Sprintf("%dh%dm", h, mm)
	}
}

// humanizeBytes renders a compact "3.4 MB" / "812 KB" / "56 B" string. The exp
// index is bounded to the unit table so a huge (or malformed) pidfile counter
// can't index past the end and panic the once-a-second dashboard render.
func humanizeBytes(n int64) string {
	const unit = 1024
	const units = "KMGTPE"
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit && exp < len(units)-1; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), units[exp])
}
