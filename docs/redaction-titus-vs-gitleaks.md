# Why Titus (and not gitleaks) for on-device secret redaction

_Date: 2026-07-07. Author: Paarth + Claude. Status: decision record — revisit only if the redaction hot path changes shape._

## TL;DR

The CLI's secret scanner must **rewrite bytes in place** on a per-event hot
path, with high precision, inside a small auditable binary. Titus
([praetorian-inc/titus](https://github.com/praetorian-inc/titus), Apache-2.0) is
an embeddable Go library that returns match spans over raw bytes and carries
the Nosey Parker rule set (~490 provider-specific, entropy-aware rules).
Gitleaks is an excellent tool for the job it was built for — scanning
repositories and failing CI — but that job is not this one. Note this scanner
is **layer 2 of 3**: the source-exclusion projection (`project.go`) and the
backend's write-boundary projection + DB CHECK constraints carry the hard
no-source guarantee; Titus + the supplemental patterns guard the *retained*
fields (prompts, command strings).

## §1. Redact-in-place vs scan-and-report

Our job (`redact.go`): take one raw transcript JSONL line, replace secret spans
with JSON-safe markers, pass the line on. That needs **byte offsets** for each
match so the splice can be narrowed to the secret token and never consume
surrounding JSON structure.

- **Titus** is a library-first scanner: feed it bytes, get back matches with
  spans. Exactly the shape `redactBytes` needs.
- **Gitleaks** is architected around scanning files/git history and emitting
  *findings* (rule id, file, line/column, the secret) for a human or CI gate.
  Its `detect` package can be bent toward arbitrary strings via Fragments, but
  you are fighting an API built for reporting, and dragging a CLI-oriented
  dependency tree into a binary whose pitch is "small, public, auditable —
  read every line that decides what leaves a developer's machine."

## §2. Rule coverage and precision lineage

Titus is Praetorian's Go port of the **Nosey Parker** rule set: ~490
provider-specific rules (AWS, GitHub, Anthropic/OpenAI, Slack, JWT, PEM, …),
tuned for precision. Gitleaks ships roughly a third as many.

Precision matters *asymmetrically* here. In CI, a false positive fails a build
and someone re-runs it. In our pipeline, a false positive **corrupts captured
data forever** — we already got burned by exactly this: an early blunt entropy
catch-all collapsed provider message ids (`msg_` / `toolu_` / `call_`) to a
constant and silently broke turn dedup (see the "Deliberately NOT here" note in
`redact.go`). High-precision provider rules plus a small supplemental generic
layer (layer 3) is the right split; a looser rule set would push more work onto
hand-written patterns.

## §3. Failure isolation

Titus engine init is once per process; if it ever fails, the supplemental
regex layer still runs — capture is never blocked and never ships unscrubbed
(**fail-open for availability, fail-closed for content**; `scrubEvent` in
`redact.go` additionally drops the whole payload if redaction ever produces
unparseable JSON). This posture is natural with an in-process library; it is
awkward with anything shaped like a subprocess-oriented tool.

## §4. The honest counterpoint

Gitleaks is far more battle-tested and more recognizable — "we run gitleaks"
lands instantly in a security review; "we run Titus" needs the Nosey Parker
footnote. If a reviewer pushes, the answer is:

1. Same category of engine (regex + entropy provider rules), **larger** rule
   set, precision-tuned lineage.
2. The secret scanner is one of three layers, and not the one the "we never
   see your source" guarantee rests on — that is the default-deny field
   projection (`project.go`, mirrored server-side) plus the teams database's
   no-source CHECK constraints, all of which are inspectable.
3. This repo is public precisely so their team can audit the actual rules and
   the actual splice logic rather than trust a brand name.
