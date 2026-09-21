#!/usr/bin/env bash
# Toolchain drift guard. Runs in CI's `lint` job, before staticcheck.
#
# go.mod's `go` directive and the staticcheck pin in .github/workflows/ci.yml
# are one pair: staticcheck can only decode standard-library export data up to
# some Go version, so a directive newer than the pin makes staticcheck die on
# stdlib packages with a message that names none of our files.
#
# The known-good pair is recorded HERE, and only here. This is deliberately not
# a staticcheck-version -> max-Go compatibility table: such a table needs an
# edit on every staticcheck release and rots into a confident lie. This records
# one pair that was observed to work, and notices when either half moves.
#
# When you change either half, verify they still work together and update the
# matching line below in the same commit.
set -euo pipefail

EXPECTED_GO_DIRECTIVE=1.27.0
EXPECTED_STATICCHECK_PIN=v0.8.1

cd "$(dirname "$0")/.."

workflow=.github/workflows/ci.yml
actual_go="$(awk '$1 == "go" { print $2; exit }' go.mod)"
mapfile -t pins < <(grep -oE 'cmd/staticcheck@[^ ]+' "$workflow" | sed 's|.*@||')

if [ "${#pins[@]}" -ne 1 ]; then
  echo "::error::expected exactly one 'cmd/staticcheck@<version>' in $workflow, found ${#pins[@]}: ${pins[*]-none}" >&2
  echo "This guard reads the pin straight off the run line. Keep it to one, or teach $0 about the others." >&2
  exit 1
fi
actual_staticcheck="${pins[0]}"

go_ok=true; sc_ok=true
[ "$actual_go" = "$EXPECTED_GO_DIRECTIVE" ] || go_ok=false
[ "$actual_staticcheck" = "$EXPECTED_STATICCHECK_PIN" ] || sc_ok=false

if $go_ok && $sc_ok; then
  echo "toolchain pair unchanged: go $actual_go + staticcheck $actual_staticcheck"
  exit 0
fi

{
  echo "::error::toolchain pair drifted — go.mod's \`go\` directive and the staticcheck pin no longer match the pair recorded in $0"
  echo
  echo "  recorded (known to work together):  go ${EXPECTED_GO_DIRECTIVE}  +  staticcheck ${EXPECTED_STATICCHECK_PIN}"
  echo "  actual now:                         go ${actual_go}  +  staticcheck ${actual_staticcheck}"
  echo
  $go_ok || echo "  MOVED: the \`go\` directive in go.mod (${EXPECTED_GO_DIRECTIVE} -> ${actual_go})"
  $sc_ok || echo "  MOVED: the staticcheck pin in ${workflow} (${EXPECTED_STATICCHECK_PIN} -> ${actual_staticcheck})"
  echo
  if ! $go_ok; then
    echo "If you did not edit go.mod yourself, a DEPENDENCY BUMP moved it. Go raises the"
    echo "main module's \`go\` directive to the highest any dependency requires, so a"
    echo "Dependabot commit that touches no Go code can raise it silently — that is exactly"
    echo "what #222 (titus v1.2.7 -> v1.2.9, which declares go 1.27.0) did on 2026-09-18."
    echo "Check the go.mod diff of the dependency you just bumped."
    echo
  fi
  echo "What to do:"
  echo "  1. Run the lint job's staticcheck step against the new pair:"
  echo "       go run honnef.co/go/tools/cmd/staticcheck@${actual_staticcheck} ./..."
  echo "  2. If it reports \"export data version N is greater than maximum supported version\""
  echo "     for stdlib packages (internal/byteorder and friends), the pin is too old for this"
  echo "     Go: raise it in ${workflow} — that is what #238 had to do reactively."
  echo "     Any other failure is a real finding in our code; fix that instead."
  echo "  3. Record the pair that works by editing EXPECTED_GO_DIRECTIVE /"
  echo "     EXPECTED_STATICCHECK_PIN at the top of $0, in the SAME commit."
} >&2
exit 1
