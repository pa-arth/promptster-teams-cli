package normalize

import "strings"

// credential_keys — harvest the KEY NAMES out of a credential file the agent
// read, on-device, and discard everything else.
//
// WHY. The backend already reports "an agent opened acme-api/.env". That is a
// notification, not a task: nobody can act on it without knowing which keys are
// in the file. "…and it holds STRIPE_SECRET_KEY and DATABASE_URL" is a rotation
// list. The names are the entire deliverable.
//
// THE PRIVACY LINE, and why it holds structurally rather than by care:
//
//   - We never open a file. The harvest runs on content the harness ALREADY
//     handed the hook (Claude Code's Read tool_response carries the body, which
//     today is measured for length and thrown away). No filesystem access, no
//     TOCTOU window, no reading a file the agent didn't actually read.
//   - The value side is never bound to a variable. Every parse below is
//     `strings.Cut`, whose second return is discarded at the call site. There is
//     no code path in this file that can retain, log, or return a right-hand
//     side, so "we might accidentally send a secret" is not a review question —
//     it is unrepresentable.
//   - A name must look like an identifier (identifierRe) and be short
//     (maxKeyNameLen). This is NOT what makes the harvest safe — the Cut above
//     is. It is a second, independent filter so that a malformed or unexpected
//     file shape degrades to "no names" instead of to "an arbitrary string".
//
// The shape rules are in LOCKSTEP with the backend's
// TEAMS_STRING_ARRAY_CLAMPS.file_read.credentialKeys
// (packages/shared/src/eventFieldProjection.ts), which re-applies all of them
// server-side for older clients. Change one, change both.
//
// SCOPE, deliberately narrow. Only file classes that are genuinely line-oriented
// `KEY=VALUE` are harvested. An SSH private key or a PKCS#12 bundle has no
// left-hand side to take, and guessing at one would mean parsing key material —
// exactly the thing this file exists not to do. The backend's much larger
// path ruleset still decides what counts as a FINDING; this only decides what
// can be usefully NAMED.

const (
	// maxKeyNames caps names per file. Mirrors the backend clamp and the
	// contract's .max(40). A rotation list past this is not a list any more.
	maxKeyNames = 40
	// maxKeyNameLen admits every plausible env-var name and rejects the bulk of
	// API-key VALUES, which mostly run 40-200 chars.
	maxKeyNameLen = 64
	// maxScanLines bounds work on a pathological file. A dotenv with more than
	// this many lines is not a dotenv; we stop rather than walk a 100MB blob.
	maxScanLines = 2000
)

// isCredentialKeyValueFile reports whether `path` names a file whose contents
// are line-oriented KEY=VALUE (or INI key = value) credential material.
//
// Matching is on the normalized path's tail, so it is independent of where the
// repo or home directory sits. Deliberately excluded: *.example / *.sample /
// *.template dotenvs (they hold placeholders, so their names are noise, and the
// backend classifies them `low` — not a finding at all).
func isCredentialKeyValueFile(path string) bool {
	p := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(path), "\\", "/"))
	if p == "" {
		return false
	}
	base := p
	if i := strings.LastIndex(p, "/"); i >= 0 {
		base = p[i+1:]
	}

	// dotenv family: .env, .env.local, .env.production, api.env …
	if base == ".env" || strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".env") {
		// Placeholders, not secrets.
		for _, skip := range []string{".example", ".sample", ".template", ".dist", ".defaults"} {
			if strings.HasSuffix(base, skip) {
				return false
			}
		}
		return true
	}

	// AWS shared credentials/config — INI, `aws_access_key_id = …`.
	if strings.HasSuffix(p, ".aws/credentials") || strings.HasSuffix(p, ".aws/config") {
		return true
	}

	// Package-registry auth files — both are `key=value`.
	if base == ".npmrc" || base == ".pypirc" {
		return true
	}

	return false
}

// HarvestCredentialKeyNames returns the identifier-shaped key NAMES on the left
// of each assignment in `content`, when `path` names a KEY=VALUE credential
// file. Returns nil for anything else — including a credential file that yielded
// no usable names.
//
// nil is load-bearing: the caller omits the field entirely rather than emitting
// an empty array, because downstream ABSENT means "this CLI did not harvest" and
// EMPTY would mean "we looked inside and the file had no keys". On a surface
// whose job is telling someone which keys to rotate, the second is a lie the
// first is not.
func HarvestCredentialKeyNames(path, content string) []string {
	if content == "" || !isCredentialKeyValueFile(path) {
		return nil
	}

	var names []string
	seen := make(map[string]struct{})

	for i, line := range strings.Split(content, "\n") {
		if i >= maxScanLines || len(names) >= maxKeyNames {
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Comments (`#` dotenv, `;` INI) and INI section headers carry no keys.
		if line[0] == '#' || line[0] == ';' || line[0] == '[' {
			continue
		}
		// `export FOO=bar` is valid dotenv.
		line = strings.TrimPrefix(line, "export ")

		// THE line that makes this file safe: the value is the blank identifier.
		// It is never named, never stored, never returned.
		name, _, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		if !isIdentifierName(name) {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}

	if len(names) == 0 {
		return nil
	}
	return names
}

// isIdentifierName reports whether s is a plausible env-var / INI key name:
// leading letter or underscore, then word characters, within the length cap.
//
// Hand-rolled rather than a regexp because this runs on every line of every
// credential file read, in a hook on the engineer's critical path.
//
// `-` and `.` are excluded on purpose. Both are legal in some exported names,
// but near-universal in secret VALUES, and the exclusion costs almost nothing on
// real dotenv files. Note what this does NOT do: it cannot tell a name from a
// value (`AKIAIOSFODNN7EXAMPLE` satisfies it and is a real AWS key id). The
// guarantee comes from strings.Cut above, not from here.
func isIdentifierName(s string) bool {
	if s == "" || len(s) > maxKeyNameLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
			continue
		case c >= '0' && c <= '9':
			if i == 0 {
				return false // a leading digit is not an identifier
			}
		default:
			return false
		}
	}
	return true
}
