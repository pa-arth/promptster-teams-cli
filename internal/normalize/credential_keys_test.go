package normalize

import (
	"encoding/json"
	"strings"
	"testing"
)

// A stand-in for the kind of value a real dotenv holds. Every test that feeds a
// file body uses THIS string on the right of the `=`, and then asserts it does
// not appear anywhere in the output. If the harvest ever starts keeping a
// right-hand side, one of these fails loudly instead of shipping a secret.
const valueCanary = "sk_live_51PROMPTSTER_LEAK_CANARY_abc123"

func TestHarvestKeepsNamesAndNeverTheValue(t *testing.T) {
	body := strings.Join([]string{
		"# local overrides",
		"STRIPE_SECRET_KEY=" + valueCanary,
		"DATABASE_URL=postgres://u:" + valueCanary + "@localhost/db",
		"export RESEND_API_KEY=" + valueCanary,
		"",
		"SENTRY_DSN = " + valueCanary,
	}, "\n")

	got := HarvestCredentialKeyNames("acme-api/.env", body)
	want := []string{"STRIPE_SECRET_KEY", "DATABASE_URL", "RESEND_API_KEY", "SENTRY_DSN"}
	assertNames(t, got, want)

	for _, n := range got {
		if strings.Contains(n, valueCanary) {
			t.Fatalf("a value survived the harvest: %q", n)
		}
	}
}

func TestHarvestReturnsNilForNonCredentialFiles(t *testing.T) {
	// A Go source file is full of `x := y` and `const a = b`; harvesting names
	// out of ordinary source would be both useless and a source-leak vector.
	for _, path := range []string{
		"internal/normalize/normalize.go",
		"README.md",
		"package.json",
		"src/config.ts",
		"~/.ssh/id_ed25519",
	} {
		if got := HarvestCredentialKeyNames(path, "KEY="+valueCanary); got != nil {
			t.Fatalf("%s: expected nil, got %v", path, got)
		}
	}
}

func TestHarvestSkipsPlaceholderDotenvs(t *testing.T) {
	// .env.example holds placeholders by convention, so its names are noise —
	// and the backend classifies it `low`, i.e. not a finding at all.
	for _, path := range []string{".env.example", ".env.sample", ".env.template", "web/.env.dist"} {
		if got := HarvestCredentialKeyNames(path, "STRIPE_SECRET_KEY=xxx"); got != nil {
			t.Fatalf("%s: expected nil, got %v", path, got)
		}
	}
}

func TestHarvestHandlesTheINICredentialFiles(t *testing.T) {
	body := strings.Join([]string{
		"[default]",
		"aws_access_key_id = AKIAIOSFODNN7EXAMPLE",
		"aws_secret_access_key = " + valueCanary,
		"; a comment",
		"[work]",
		"aws_access_key_id = AKIAI44QH8DHBEXAMPLE",
	}, "\n")

	got := HarvestCredentialKeyNames("~/.aws/credentials", body)
	// Section headers and comments contribute nothing; the duplicate key across
	// two profiles folds to one name.
	assertNames(t, got, []string{"aws_access_key_id", "aws_secret_access_key"})
}

func TestHarvestRejectsNonIdentifierLeftSides(t *testing.T) {
	body := strings.Join([]string{
		"GOOD_KEY=" + valueCanary,
		"9LEADING_DIGIT=x",    // not an identifier
		"has space=x",         // a sentence, not a key
		"DASHED-KEY=x",        // `-` excluded: near-universal in values
		"dotted.key=x",        // same
		"=" + valueCanary,     // empty left side
		"no equals sign here", // no assignment at all
		strings.Repeat("A", maxKeyNameLen+1) + "=x", // over the length cap
	}, "\n")

	assertNames(t, HarvestCredentialKeyNames(".env", body), []string{"GOOD_KEY"})
}

func TestHarvestCapsTheCount(t *testing.T) {
	var lines []string
	for i := 0; i < maxKeyNames+25; i++ {
		lines = append(lines, "KEY_"+itoa(i)+"="+valueCanary)
	}
	got := HarvestCredentialKeyNames(".env", strings.Join(lines, "\n"))
	if len(got) != maxKeyNames {
		t.Fatalf("expected the cap %d, got %d", maxKeyNames, len(got))
	}
	if got[0] != "KEY_0" {
		t.Fatalf("cap should keep the FIRST names, got %q first", got[0])
	}
}

func TestHarvestReturnsNilNotEmptyWhenNothingUsable(t *testing.T) {
	// The distinction the whole feature rests on. nil ⇒ the caller omits the
	// field ⇒ the backend reads "not harvested". An empty slice would serialize
	// to `[]` and claim we looked inside the file and it held no keys.
	got := HarvestCredentialKeyNames(".env", "# nothing but comments\n\n[section]\n")
	if got != nil {
		t.Fatalf("expected nil (absent), got %#v", got)
	}
	if HarvestCredentialKeyNames(".env", "") != nil {
		t.Fatal("empty content must be absent, not empty")
	}
}

// The end-to-end shape: a Read tool_response carrying a .env body must produce a
// file_read event with the NAMES and, critically, no trace of the file.
func TestNormalizeEmitsCredentialKeysWithoutTheBody(t *testing.T) {
	body := "STRIPE_SECRET_KEY=" + valueCanary + "\nDATABASE_URL=" + valueCanary + "\n"
	data := readEventData(t, "/Users/someone/repos/acme-api/.env", body)

	keys, _ := data["credentialKeys"].([]string)
	assertNames(t, keys, []string{"STRIPE_SECRET_KEY", "DATABASE_URL"})

	// Nothing in the emitted payload may contain the body. e.RawPayload still
	// holds it at this stage by design — redact.ProjectEvent clears RawPayload
	// and re-projects Data before anything is signed or sent.
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), valueCanary) {
		t.Fatalf("the file body leaked into the event payload: %s", encoded)
	}
	// The no-source contract: the body is measured, never carried.
	if _, present := data["content"]; present {
		t.Fatalf("file_read must never carry `content`, got %#v", data)
	}
	if data["contentLength"] != len(body) {
		t.Fatalf("contentLength should still be measured, got %v", data["contentLength"])
	}
}

func TestNormalizeOmitsCredentialKeysForOrdinaryReads(t *testing.T) {
	// Ordinary source is full of `const KEY = …`; it must contribute nothing.
	data := readEventData(t, "/Users/someone/repos/acme-api/src/app.ts", "const KEY = "+valueCanary)
	if _, present := data["credentialKeys"]; present {
		t.Fatalf("a non-credential read must carry NO credentialKeys key at all, got %#v", data)
	}
}

// readEventData drives the real PostToolUse path for a Read of `path` whose
// response carries `body`, and returns the emitted file_read payload.
func readEventData(t *testing.T, path, body string) map[string]interface{} {
	t.Helper()
	e, ok := normalizePostToolUseByTool(
		"Read",
		map[string]interface{}{"file_path": path},
		map[string]interface{}{
			"file": map[string]interface{}{
				"filePath": path,
				"content":  body,
				"numLines": float64(strings.Count(body, "\n")),
			},
		},
		"sess-test",
		`{"tool_name":"Read"}`,
	)
	if !ok {
		t.Fatal("expected a normalized event")
	}
	if e.Kind != "file_read" {
		t.Fatalf("expected file_read, got %q", e.Kind)
	}
	data, isMap := e.Data.(map[string]interface{})
	if !isMap {
		t.Fatalf("expected map data, got %T", e.Data)
	}
	return data
}

// ── helpers ────────────────────────────────────────────────────────────────

func assertNames(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (order matters: first-seen)", got, want)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
