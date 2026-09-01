package cli

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
	"github.com/pa-arth/promptster-teams-cli/internal/policy"
	"github.com/pa-arth/promptster-teams-cli/internal/selfupdate"
	"github.com/pa-arth/promptster-teams-cli/internal/version"
)

// cmdUpdate is the manual update path, and the escape hatch that makes the rest
// of the consent design honest.
//
// The background updater cannot ask a question — it runs in a detached daemon
// with no terminal — so every branch where it declines to install has to leave
// the engineer something to type. This is that something. It is also the only
// way an engineer who said "no" to background updates can still take a specific
// release, which is what keeps a standing "no" from meaning "stay on this build
// forever".
func cmdUpdate(args []string) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	checkOnly := fs.Bool("check", false, "Report what an update would do, install nothing")
	assumeYes := fs.Bool("yes", false, "Install without asking (for scripts and non-interactive shells)")
	enableAuto := fs.Bool("enable-auto", false, "Allow this machine to install updates in the background from now on")
	disableAuto := fs.Bool("disable-auto", false, "Stop this machine installing updates in the background")
	askEach := fs.Bool("ask-each", false, "Show a notification for each new release and let you decide")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	fmt.Println()
	fmt.Println(brandBar("update"))
	fmt.Println()

	if countTrue(*enableAuto, *disableAuto, *askEach) > 1 {
		printlnIndent(fmt.Sprintf("%s --enable-auto, --ask-each and --disable-auto are mutually exclusive.", errGlyph))
		fmt.Println()
		return 2
	}

	// The consent flags are handled first and can stand alone: `update
	// --enable-auto` is a complete instruction, and it must work even when the
	// network is down or GitHub is unreachable. Making a preference change
	// depend on a successful release check would put the fix for "my fleet is
	// stuck" behind the same failure that is often causing it.
	switch {
	case *enableAuto:
		selfupdate.SaveConsent(selfupdate.ConsentGranted, version.Version)
		printlnIndent(fmt.Sprintf("%s background updates enabled — capture will keep itself current.", okGlyph))
	case *disableAuto:
		selfupdate.SaveConsent(selfupdate.ConsentDenied, version.Version)
		printlnIndent(fmt.Sprintf("%s background updates disabled — run `promptster-teams update` when you want one.", okGlyph))
	case *askEach:
		selfupdate.SaveConsent(selfupdate.ConsentAsk, version.Version)
		printlnIndent(fmt.Sprintf("%s you'll get a notification for each new release.", okGlyph))
	}
	if (*enableAuto || *disableAuto || *askEach) && !*checkOnly && len(fs.Args()) == 0 && !*assumeYes {
		// A bare consent change is done. Fall through to the version check only
		// when the engineer also asked to update.
		fmt.Println()
		return 0
	}

	pol := updatePolicyView()

	st, err := selfupdate.CheckManual(pol)
	if err != nil {
		printlnIndent(fmt.Sprintf("%s %v", errGlyph, err))
		fmt.Println()
		return 1
	}

	switch {
	case st.Blocked != "":
		printlnIndent(fmt.Sprintf("%s %s", warnGlyph, st.Blocked))
		fmt.Println()
		return 1
	case st.UpToDate && st.Pinned:
		printlnIndent(fmt.Sprintf("%s on %s, the version your organization pins.", okGlyph, st.Current))
		fmt.Println()
		return 0
	case st.UpToDate:
		printlnIndent(fmt.Sprintf("%s on %s, the latest release.", okGlyph, st.Current))
		fmt.Println()
		return 0
	}

	printlnIndent(fmt.Sprintf("update available: %s → %s", st.Current, st.Target))
	if st.Pinned {
		printlnIndent(dimStyle.Render("Your organization pins this version."))
	}
	printlnIndent(dimStyle.Render("Release notes: ") + bodyStyle.Render(selfupdate.ReleaseNotesURL(st.Target)))
	fmt.Println()

	if *checkOnly {
		printlnIndent(dimStyle.Render("Run `promptster-teams update` to install it."))
		fmt.Println()
		return 0
	}

	if !*assumeYes {
		if !stdinIsTTY() {
			// Refuse rather than assume. This command installs code; EOF on a
			// pipe is not agreement, and a script that wants it unattended can
			// say so with --yes.
			printlnIndent(fmt.Sprintf("%s not a terminal — re-run with --yes to install without a prompt.", errGlyph))
			fmt.Println()
			return 1
		}
		fmt.Printf("  %s Install %s now? [y/N] ", promptGlyph(), st.Target)
		answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		fmt.Println()
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes":
		default:
			printlnIndent(dimStyle.Render("Left on " + st.Current + "."))
			fmt.Println()
			return 0
		}
	}

	if err := selfupdate.ApplyManual(pol, st.Target); err != nil {
		printlnIndent(fmt.Sprintf("%s update rejected: %v", errGlyph, err))
		printlnIndent(dimStyle.Render("Nothing was installed — you are still on " + st.Current + "."))
		fmt.Println()
		return 1
	}

	printlnIndent(fmt.Sprintf("%s installed %s.", okGlyph, st.Target))
	// Capture is a SEPARATE, long-lived process that is still running the old
	// build. It picks this up on its own: the watcher re-execs into a newer
	// binary on its own disk on every poll (selfupdate.catchUpToDisk), which is
	// well under a minute of drift in practice. Saying so is better than telling
	// everyone to restart capture, which drops the Cursor rail back to EOF and
	// loses the opening prompt of any session in flight.
	printlnIndent(dimStyle.Render("Background capture will pick it up within a few minutes."))
	if !selfupdate.HasDecided() {
		fmt.Println()
		printlnIndent(dimStyle.Render("To let it update itself next time: ") + bodyStyle.Render("promptster-teams update --enable-auto"))
	}
	fmt.Println()
	return 0
}

// PromptForUpdateConsent asks the one-time question, if it is still worth
// asking, and records the answer. `login` and `start` call it.
//
// Those two commands are the ONLY moments this product can collect an answer.
// Everything else about it runs unattended: capture is a detached daemon, and
// the CLI is designed to be installed once and never typed again. So the
// question is asked where the engineer is provably standing at a keyboard, and
// then remembered forever — rather than at update time, where nobody is
// listening and the previous design's prompt silently declined itself on every
// cycle.
//
// It stays quiet in three cases, each for a different reason:
//
//   - Already answered — asking again would make a remembered preference into
//     the recurring prompt this replaced.
//   - Org-managed — the org's switch and pin decide which builds reach company
//     machines. Asking the engineer would imply their answer counts, and it
//     does not.
//   - No terminal — `login --key ...` in a provisioning script has no human to
//     answer. Leaving it unknown means the next interactive run asks, which is
//     right; writing a default would put words in someone's mouth.
func PromptForUpdateConsent() {
	if selfupdate.HasDecided() || !stdinIsTTY() {
		return
	}
	if pol := updatePolicyView(); pol != nil && pol.OrgManaged() {
		return
	}

	printlnIndent(bodyStyle.Render("How should promptster-teams handle updates?"))
	printlnIndent(dimStyle.Render("Releases are signed and verified before anything is installed."))
	fmt.Println()
	printlnIndent(bodyStyle.Render("  a") + dimStyle.Render(") ask me about each release   (default)"))
	printlnIndent(bodyStyle.Render("  y") + dimStyle.Render(") update automatically"))
	printlnIndent(bodyStyle.Render("  n") + dimStyle.Render(") never update on its own"))
	fmt.Println()
	fmt.Printf("  %s Choose [A/y/n] ", promptGlyph())
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	fmt.Println()
	if err != nil {
		// EOF mid-question is not an answer. Leave it undecided so the next
		// interactive run asks properly.
		return
	}

	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		selfupdate.SaveConsent(selfupdate.ConsentGranted, version.Version)
		printlnIndent(fmt.Sprintf("%s capture will keep itself up to date.", okGlyph))
	case "n", "no", "never":
		selfupdate.SaveConsent(selfupdate.ConsentDenied, version.Version)
		printlnIndent(fmt.Sprintf("%s staying on %s — run `promptster-teams update` when you want a newer one.", okGlyph, version.Version))
	default:
		// Empty input takes the capitalized default. This is the one place a
		// default is legitimate: the engineer saw the question.
		selfupdate.SaveConsent(selfupdate.ConsentAsk, version.Version)
		printlnIndent(fmt.Sprintf("%s you'll get a notification when a new release is available.", okGlyph))
	}
	printlnIndent(dimStyle.Render("Change it later with ") + bodyStyle.Render("promptster-teams update --enable-auto") + dimStyle.Render(" / ") + bodyStyle.Render("--ask-each") + dimStyle.Render(" / ") + bodyStyle.Render("--disable-auto") + dimStyle.Render("."))
	fmt.Println()
}

// updatePolicyView builds the org policy view the update path consults, or nil
// when this machine has no key configured.
//
// nil is the right answer for an unconfigured machine rather than an error: the
// org switch and pin are enforced when an org has stated them, and a machine
// with no key has no org to ask. It still updates — it just has nobody but the
// local consent record to answer to.
func updatePolicyView() selfupdate.PolicyView {
	token, _ := ingest.ResolveToken("")
	if token == "" {
		return nil
	}
	r := policy.NewResolver(token)
	// Refresh synchronously: this is an interactive command about to install
	// code, so it should act on the org's CURRENT answer rather than a cached
	// one. A failed refresh is not fatal — Refresh retains the last known intent
	// — but an org that disabled updates a minute ago should be obeyed now, not
	// at the daemon's next poll.
	r.Refresh()
	return r
}

// countTrue counts how many of the given flags are set, so mutually exclusive
// options can be rejected as a group rather than in pairs.
func countTrue(flags ...bool) int {
	n := 0
	for _, f := range flags {
		if f {
			n++
		}
	}
	return n
}
