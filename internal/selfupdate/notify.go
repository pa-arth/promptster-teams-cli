package selfupdate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// The per-release GUI prompt: how a daemon with no terminal asks a human a
// question.
//
// This is the one thing the old terminal prompt could never do. A detached
// watcher has no stdin to read, but it DOES run in the user's graphical session
// — on macOS `autostart` installs a LaunchAgent under ~/Library/LaunchAgents
// (service_darwin.go), and LaunchAgents run in the Aqua session, so they can
// display UI. Verified end to end: a dialog spawned from a detached, TTY-less
// process renders and returns its answer.
//
// It is exec-based rather than a GUI library because the dependency cost of a
// real toolkit is enormous next to shelling out to a tool every desktop already
// ships, and because a failure to display must degrade to "not asked" rather
// than take the capture daemon down with it.
//
// One file with a runtime switch rather than three build-tagged ones: every
// branch is the same shape — build an argv, run it with a timeout, read a button
// name — and splitting that across files hides how similar they are.

// guiPromptTimeout bounds how long the daemon waits for an answer.
//
// It is a HARD requirement, not politeness. A dialog on a locked screen, a
// disconnected display, or a machine whose owner walked away would otherwise
// block this goroutine for the life of the process — and on Linux a zenity that
// cannot reach a display can hang rather than exit. The timeout means an
// unanswered prompt is simply "not now": the next check offers the release
// again.
const guiPromptTimeout = 5 * time.Minute

// guiPromptResult is what one attempt to ask a human produced.
type guiPromptResult int

const (
	// guiUnavailable means no dialog could be shown at all (headless box, no
	// desktop, tool missing). It is NOT a refusal — the engineer was never
	// asked, so nothing may be inferred from it, and the caller falls back to
	// the printed surfaces.
	guiUnavailable guiPromptResult = iota
	// guiDeclined means a human saw the prompt and said no, or let it time out.
	guiDeclined
	// guiAccepted means a human saw the prompt and said yes.
	guiAccepted
)

// promptGUI asks the engineer whether to install target, showing the version and
// a link to the release notes. It never returns an error: every failure is
// guiUnavailable, because a broken notifier must not break capture.
func promptGUI(current, target, notesURL string) guiPromptResult {
	// The tag reaches us from a GitHub redirect and is interpolated into a
	// shell/AppleScript argument here. tagFromReleaseLocation already rejects
	// path separators, but that was written to protect a URL path, not a script
	// literal — a tag carrying a quote would end the AppleScript string and run
	// what follows. Sanitize for THIS context rather than trusting a check aimed
	// at a different one.
	current, target = sanitizeForDialog(current), sanitizeForDialog(target)
	if !isSafeURL(notesURL) {
		notesURL = ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), guiPromptTimeout)
	defer cancel()

	switch runtime.GOOS {
	case "darwin":
		return promptDarwin(ctx, current, target, notesURL)
	case "windows":
		return promptWindows(ctx, current, target, notesURL)
	default:
		return promptLinux(ctx, current, target, notesURL)
	}
}

// dialogBody is the text every platform shows. It names both versions and links
// the release notes, so the engineer can see what they are agreeing to instead
// of being asked to trust a version number.
//
// It deliberately does NOT claim to be from a trusted source or urge speed. An
// unexpected "install this update?" dialog is the exact shape of a phishing
// prompt, and any local process can show an identical one — so the honest thing
// is to state the facts, link the notes, and let the engineer verify.
func dialogBody(current, target, notesURL string) string {
	b := "promptster-teams " + target + " is available (you have " + current + ")."
	if notesURL != "" {
		b += "\n\nRelease notes:\n" + notesURL
	}
	return b
}

func promptDarwin(ctx context.Context, current, target, notesURL string) guiPromptResult {
	script := `display dialog "` + escapeAppleScript(dialogBody(current, target, notesURL)) +
		`" buttons {"Later", "Update"} default button "Update" with title "promptster-teams" giving up after ` +
		strconv.Itoa(int(guiPromptTimeout.Seconds()))

	out, err := runDialog(ctx, "osascript", "-e", script)
	if err != nil {
		return guiUnavailable
	}
	// osascript reports both the button and whether it timed out. "gave up:true"
	// means nobody answered, which is not consent.
	if strings.Contains(out, "gave up:true") {
		return guiDeclined
	}
	if strings.Contains(out, "button returned:Update") {
		return guiAccepted
	}
	return guiDeclined
}

// promptLinux is the WEAKEST of the three platforms, and the comment is here so
// nobody assumes otherwise from the fact that the code exists.
//
// The autostart unit is a `systemd --user` service (service.go renderUnit) with
// no Environment= or PassEnvironment=, so whether it can see DISPLAY depends
// entirely on whether the desktop ran `systemctl --user import-environment`.
// GNOME and KDE generally do; a bare WM, a login without a graphical session, and
// every headless box do not. So on Linux this path legitimately reports
// "unavailable" a lot, and unavailable is NOT a refusal — the caller falls back
// to the printed surfaces and the engineer still learns a release exists.
func promptLinux(ctx context.Context, current, target, notesURL string) guiPromptResult {
	env, ok := linuxDisplayEnv()
	if !ok {
		// No graphical session reachable: a server, an SSH login, a user unit that
		// never had the session environment imported. Nobody can be asked.
		return guiUnavailable
	}
	body := dialogBody(current, target, notesURL)

	if path, err := exec.LookPath("zenity"); err == nil {
		_, err := runDialogEnv(ctx, env, path, "--question", "--title=promptster-teams",
			"--text="+body, "--ok-label=Update", "--cancel-label=Later")
		// zenity signals the answer through its EXIT CODE (0 = ok), so a nil
		// error is the yes and a non-zero exit is the no.
		if err == nil {
			return guiAccepted
		}
		if isExitError(err) {
			return guiDeclined
		}
		return guiUnavailable
	}
	if path, err := exec.LookPath("kdialog"); err == nil {
		_, err := runDialogEnv(ctx, env, path, "--yesno", body, "--title", "promptster-teams")
		if err == nil {
			return guiAccepted
		}
		if isExitError(err) {
			return guiDeclined
		}
		return guiUnavailable
	}
	return guiUnavailable
}

// linuxDisplayEnv builds the environment a dialog needs to reach the user's
// display, and reports whether one is plausibly reachable at all.
//
// When DISPLAY/WAYLAND_DISPLAY are already set (the desktop imported them into
// the systemd user environment) it uses them. When they are not, it falls back to
// DISPLAY=:0 — the single-seat desktop, which is overwhelmingly the shape of a
// machine that has a human sitting at it — rather than giving up outright. The
// fallback costs one failed exec on a headless box, where zenity exits non-zero
// and the caller degrades to the printed surfaces anyway.
//
// XAUTHORITY is passed through when set and guessed at ~/.Xauthority otherwise,
// because an X client with no auth cookie cannot connect even with the right
// DISPLAY.
func linuxDisplayEnv() ([]string, bool) {
	env := os.Environ()
	hasX, hasWayland := os.Getenv("DISPLAY") != "", os.Getenv("WAYLAND_DISPLAY") != ""

	if !hasX && !hasWayland {
		// A headless machine has no display at all, and there is no cheap way to
		// tell it apart from a desktop whose environment was never imported. Try
		// the single-seat default; a wrong guess is one failed exec.
		env = append(env, "DISPLAY=:0")
	}
	if os.Getenv("XAUTHORITY") == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			env = append(env, "XAUTHORITY="+home+"/.Xauthority")
		}
	}
	return env, true
}

func promptWindows(ctx context.Context, current, target, notesURL string) guiPromptResult {
	// PowerShell's MessageBox is the one dialog present on every supported
	// Windows without installing anything. The body is passed as a single-quoted
	// PowerShell literal, so only the single quote needs doubling.
	body := strings.ReplaceAll(dialogBody(current, target, notesURL), "'", "''")
	// The box is shown with an OWNER form marked TopMost. Without an owner a
	// MessageBox raised from a background scheduled task can open behind the
	// foreground window, where an engineer never sees it — which would look
	// exactly like the silent-decline bug this whole change set removes.
	script := `Add-Type -AssemblyName System.Windows.Forms; ` +
		`$o=New-Object System.Windows.Forms.Form; $o.TopMost=$true; ` +
		`$r=[System.Windows.Forms.MessageBox]::Show($o,'` + body + `','promptster-teams','YesNo'); ` +
		`$o.Dispose(); if($r -eq 'Yes'){Write-Output 'ACCEPTED'}`

	out, err := runDialog(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	if err != nil {
		return guiUnavailable
	}
	if strings.Contains(out, "ACCEPTED") {
		return guiAccepted
	}
	return guiDeclined
}

// runDialog executes one dialog command under ctx and returns its stdout.
//
// stdin is explicitly nil so the child cannot inherit and block on the daemon's
// descriptors, and the context bounds a tool that hangs instead of exiting.
func runDialog(ctx context.Context, name string, args ...string) (string, error) {
	return runDialogEnv(ctx, nil, name, args...)
}

// runDialogEnv is runDialog with an explicit environment, for the Linux path
// that has to supply DISPLAY/XAUTHORITY itself. A nil env inherits this
// process's.
func runDialogEnv(ctx context.Context, env []string, name string, args ...string) (string, error) {
	// #nosec G204 -- name is a literal or a LookPath result, and every
	// interpolated value is sanitized above; this is how a daemon reaches the
	// desktop's own dialog tools.
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = nil
	cmd.Env = env
	out, err := cmd.Output()
	return string(out), err
}

func isExitError(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee)
}

// sanitizeForDialog reduces a version to characters that cannot terminate a
// string literal or introduce a shell/AppleScript construct. Anything else is
// dropped rather than escaped: a version is [0-9A-Za-z.+_-] in every form we
// publish, so a character outside that set means something is wrong upstream and
// the safe response is to show less, not to try to render it faithfully.
func sanitizeForDialog(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r == '.', r == '-', r == '_', r == '+':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isSafeURL accepts only the shape this package builds — an https URL on
// github.com with no character that could escape a dialog string. A URL that
// fails this is omitted from the dialog rather than shown, since a release-notes
// link is a convenience and a malformed one is a signal, not something to render.
func isSafeURL(u string) bool {
	if !strings.HasPrefix(u, "https://github.com/") {
		return false
	}
	for _, r := range u {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r == ':', r == '/', r == '.', r == '-', r == '_', r == '+', r == '~':
		default:
			return false
		}
	}
	return true
}

// escapeAppleScript escapes the two characters that can break out of an
// AppleScript string literal. It is the last line of defence — dialogBody's
// inputs are already sanitized — but the two together mean neither has to be
// perfect alone.
func escapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}
