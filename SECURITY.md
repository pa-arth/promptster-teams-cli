# Security Policy

`promptster-teams` runs continuously on developer machines and reads AI-coding
transcripts, so we treat its security seriously and welcome scrutiny — this
repository is public precisely so it can be audited.

## Reporting a vulnerability

Please report suspected vulnerabilities **privately** — do not open a public
issue for anything security-sensitive.

- **Preferred:** GitHub → this repo's **Security** tab → **Report a
  vulnerability** (private advisory). This routes directly to the maintainers
  and keeps the report confidential until a fix ships.
- If you cannot use GitHub advisories, contact the maintainer listed in
  `go.mod` / the GitHub org and request a private channel.

Please include: affected version (`promptster-teams version`), platform, a
description, and a reproduction if possible.

We aim to acknowledge within **3 business days** and to provide a remediation
timeline after triage. We will credit reporters who wish to be named once a fix
is released.

## Supported versions

This is pre-1.0 software; only the **latest released version** receives security
fixes. Upgrade via `npm update -g @promptster/teams-cli` or re-running the
installer.

## What's in scope

- The on-device redaction pipeline (`redact.go`, `project.go`) — anything that
  could cause **source code, secrets, or PII to leave the machine** unredacted.
- The signing/chaining path (`signing.go`) — signature forgeability or chain
  tampering that isn't detected.
- Credential handling (`credentials.go`) — leakage of the developer key or the
  Ed25519 private seed.
- The distribution path — the installer (`install.sh`), the release pipeline,
  and the npm package, including checksum/provenance bypass.

## What's out of scope

- The private Promptster backend (report those through the same channel, but
  they are not part of this repository).
- Findings that require an already-compromised machine (e.g. root reading
  `~/.promptster-teams/session.key`, which is mode `0600` by design).
- The intentional **presence heartbeat** (documented in the README) — it
  carries no transcript content by construction and is enforced by
  `presence_test.go`.

## Verifying a release

Every release publishes `SHA256SUMS`. `install.sh` verifies the downloaded
binary against it before execution. The npm package is published with **build
provenance** (SLSA attestation) — verify with `npm audit signatures`.
