package cli

import (
	"fmt"
	"strings"

	"github.com/pa-arth/promptster-teams-cli/internal/discovery"
)

// cmdDiscover performs metadata-only discovery. Setup remains user-scoped:
// another home can have different filesystem permissions, Keychain access,
// launch services, and product credentials, so this process must not reach in
// and install hooks or copy the Promptster key on that user's behalf.
func cmdDiscover() {
	homes := discovery.AdditionalHomes()
	fmt.Println()
	fmt.Println(brandBar("discover"))
	fmt.Println()
	if len(homes) == 0 {
		printlnIndent(fmt.Sprintf("%s no additional Claude, Codex, or Cursor homes found", okGlyph))
		printlnIndent(dimStyle.Render("Discovery checks directory metadata only; private or custom locations can be set up by running `promptster-teams login` inside that environment."))
		fmt.Println()
		return
	}

	printlnIndent(fmt.Sprintf("%s found %d additional AI environment(s)", warnGlyph, len(homes)))
	for _, home := range homes {
		state := "Promptster not detected"
		if home.PromptsterEnrolled {
			state = "Promptster state found — run `promptster-teams doctor` as that user to verify it is sending"
		}
		fmt.Println()
		printlnIndent(bodyStyle.Render(home.Path))
		printlnIndent(dimStyle.Render("Products: " + strings.Join(home.Products, ", ")))
		printlnIndent(dimStyle.Render(state))
	}
	fmt.Println()
	printlnIndent(bodyStyle.Render("For each environment not yet enrolled, sign in as that OS user and run:"))
	printlnIndent(bodyStyle.Render("promptster-teams login"))
	printlnIndent(dimStyle.Render("Use the same developer key to link it to the same Promptster account. Each installation gets its own health identity, queue, and signing chain."))
	fmt.Println()
}
