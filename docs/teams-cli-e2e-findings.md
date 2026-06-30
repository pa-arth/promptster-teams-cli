# promptster-teams-cli → Supabase e2e: findings & required changes

_Date: 2026-06-29. Author: Claude (co-founder session). Status: blocked on a product-direction decision (see §0)._

## TL;DR

> **UPDATE 2026-06-29:** Direction **(A)** chosen and **implemented + verified end-to-end**. A teams
> ingest path now exists (`POST /v1/teams/ingest`) that authenticates with an org API key, writes to
> the dedicated teams DB (`eeqtxycyglfyqzfwwuli`), and pins the device pubkey so signatures verify.
> Live CLI→backend→teams-DB run passed. See **§6**. The three findings below are the original
> diagnosis (kept for the record); F1/F2/F3 are now addressed on backend branch `teams-ingest`
> (uncommitted).

A true end-to-end run (CLI → backend → Supabase) **could not originally land teams data in the
teams DB**, because the backend half of the teams product did not exist. The teams CLI was
built to POST the hiring backend's event contract, but:

1. The only ingest endpoint writes to the **main hiring DB**, not the teams DB.
2. There is **no teams auth/identity** — ingest only accepts hiring *candidate keys*.
3. The device signing pubkey the CLI sends is **ignored**, so signatures are never verified.

None of these are schema bugs in the teams DB — that DB already has the full mirror schema
(`raw_events`, `sessions`, `timeline_events`, …). The gap is backend wiring (routing + auth +
pubkey pinning).

---

## §0. The decision that unblocks everything

**Where should teams capture data live, and under what identity?** Pick one:

- **(A) Dedicated teams DB (recommended).** Stand up a teams ingest path that writes to
  `TEAMS_DATABASE_URL` (`eeqtxycyglfyqzfwwuli`). Matches the README's promise that teams capture
  is *separate* from the hiring product, and matches the intent already encoded in `.env` and this
  repo's `.mcp.json`. Most work, cleanest product.
- **(B) Shared main DB, teams-scoped org.** Point teams events at the existing
  `zgxqvcguovsygzdymbci` ingest but give teams its own org + a non-candidate token type. Less
  work; commingles teams + hiring data (contradicts README's separation claim).
- **(C) Decide teams is not ready.** Treat the CLI as capture-only with a documented "no backend
  yet" status; defer ingest. (The README already half-says this.)

Everything in §3 is written against **(A)** since that's the recommendation; call out if you want
(B)/(C) and I'll re-scope.

---

## §1. Verified architecture (what actually happens today)

| Hop | Detail | Source |
|---|---|---|
| CLI ships event | `POST {PROMPTSTER_TEAMS_API_URL}/v1/hooks/ingest`, header `X-API-Key: {TOKEN}`, `X-Promptster-Device-Pubkey: {b64}` | `ingest.go:36-65`, `api.go` |
| Default target | `https://api.promptster.ai` (overridable) | `api.go:10` |
| Backend route | Fastify, `promptster-backend`, port 3001, `POST /v1/hooks/ingest` | `apps/api/src/routes/hooks.ts:117` |
| Backend DB | `app.db = createDb(env.DATABASE_URL)` → **`zgxqvcguovsygzdymbci`** (main, us-east-2) | `apps/api/src/index.ts:40`, `plugins/db.ts:11` |
| Teams DB | `eeqtxycyglfyqzfwwuli` (us-west-2) = `TEAMS_DATABASE_URL` — **referenced nowhere in source** | grep: only in `.env` |
| Writes | `sessions` (upsert), `raw_events` (insert, idempotent on `event.id`), `timeline_events`, `decisions` (if kind=decision_event) | `hooks.ts:200-311` |

## §2. The three findings

### F1 — Teams data goes to the wrong DB (CRITICAL)
The ingest handler writes through a single `app.db` connection bound to `DATABASE_URL`
(main hiring DB). `TEAMS_DATABASE_URL` / `eeqtxycyglfyqzfwwuli` is never used in code. There is no
teams-specific ingest route and no second DB client. **Result:** any event the teams CLI manages
to send lands in the hiring DB, commingled with candidate assessment data — directly contradicting
the README ("Promptster's hiring product is a separate, private codebase; none of its … logic
exists here").

### F2 — No teams auth/identity model (CRITICAL)
`preHandler: requireCandidateAuth` (`apps/api/src/plugins/auth.ts:108-142`) validates `X-API-Key`
by exact match against `candidate_keys.key` where `status='active'` and `expires_at > now()`, and
derives `org_id` from `candidate_keys.org_id`. Candidate keys are **per-hiring-assessment**
artifacts. The teams CLI has no candidate key, no redeem flow (`teams.go:loadSession` explicitly:
"no server round trip, no key, no consent gate"), and `PROMPTSTER_TEAMS_TOKEN` has nothing to
validate against. **Result:** the teams CLI cannot authenticate to the only existing ingest
endpoint at all.

### F3 — Device signing pubkey ignored; signatures never verified (HIGH)
`hooks.ts` only *reads* `sessions.signing_pubkey` (line 160/171) to decide whether to verify; it
never writes it, and it never reads the `X-Promptster-Device-Pubkey` header (grep: that header name
appears nowhere in backend source). `signing_pubkey` is written only in the hiring candidate-redeem
flow (`candidate.ts:225`), which teams never calls. **Result:** teams sessions keep
`signing_pubkey = NULL` → `sig_verified = NULL` for every event → the tamper-evident chain the
README advertises is unverified server-side.

---

## §3. Required changes for option (A) — teams ingest → teams DB

### Backend (`promptster-backend`) — the bulk of the work
1. **Second DB client.** Build a `teamsDb = createDb(env.TEAMS_DATABASE_URL)` and decorate it
   (`app.teamsDb`), or run a separate teams API process pointed at `TEAMS_DATABASE_URL`.
2. **Teams ingest route.** `POST /v1/teams/ingest` (or route by token type on the existing path)
   that writes `sessions` / `raw_events` / `timeline_events` to `teamsDb`. Reuse the existing
   column mapping in `hooks.ts:200-271` verbatim — schema matches.
3. **Teams auth.** New `requireTeamsAuth` preHandler. Validate `PROMPTSTER_TEAMS_TOKEN` against a
   teams token model (see schema below); resolve `org_id` from it. Do **not** reuse `candidate_keys`.
4. **Pin device pubkey.** On first teams event for a session, write `X-Promptster-Device-Pubkey`
   into `sessions.signing_pubkey`; reject later pubkey changes for that device/session; then
   `sig_verified` becomes meaningful. (Fixes F3.)
5. **`org_id` provenance.** Teams sessions need an org. Decide device→org mapping (token carries
   org, or device-enrollment table).

### Schema (teams DB `eeqtxycyglfyqzfwwuli`) — small; mirror schema already present
The capture tables already exist and match. New objects needed only for teams auth/identity:
- [ ] `team_ingest_keys` (or reuse `org_api_keys` with a teams scope): `id, org_id, token_hash,
      prefix, label, created_at, revoked_at, last_used_at`. Hash the token (don't store plaintext
      like `candidate_keys` does).
- [ ] `team_devices` (optional but recommended): `device_id (PK, the CLI's dev-XXXX), org_id,
      signing_pubkey, first_seen_at, last_seen_at` — to pin pubkey per device across sessions, not
      just per session. (Today `sessions.signing_pubkey` is per-session only.)
- [ ] Decide retention for teams `raw_events` (the teams DB has `org_branding.retention_days`).
- [ ] Confirm RLS posture on the teams DB matches the main DB (all teams tables show `rls_enabled`).

### CLI (`promptster-teams-cli`) — minor
- [ ] Default `PROMPTSTER_TEAMS_API_URL` / `ingestEndpoint()` to the teams route once it exists
      (today defaults to `api.promptster.ai` + `/v1/hooks/ingest`).
- [ ] `doctor` should ping the ingest endpoint and surface auth failures (currently only checks env
      presence).
- [ ] Consider a one-time device enrollment call so the backend can pin the pubkey before the first
      event (removes the first-event-unsigned gap).

---

## §4. How to actually run the e2e (once direction is chosen)

Local, fully sandboxed — overrides keep it off real state:
```
CLAUDE_CONFIG_DIR=<tmp>/claude   CODEX_HOME=<tmp>/codex
PROMPTSTER_STATE_DIR=<tmp>/state PROMPTSTER_BUFFER_PATH=<tmp>/buffer.jsonl
PROMPTSTER_TEAMS_WATCH_DIR=<tmp>/repo
PROMPTSTER_TEAMS_API_URL=http://localhost:3001  PROMPTSTER_TEAMS_TOKEN=<seeded token>
```
1. Run `promptster-backend` locally (`pnpm dev`, port 3001) pointed at the chosen DB.
2. Seed an org + a valid token (teams token for (A); or `pnpm db:seed:demo` gives candidate key
   `PST-DEMO-0001` against the **main** DB for a wrong-DB demonstration).
3. `promptster-teams-cli watch` with the env above; drop a synthetic Claude transcript
   (`<tmp>/claude/projects/<munged>/<uuid>.jsonl`, timestamps within last 2 min — watcher cutoff is
   `StartedAt − 2m`, see `cmd_claude_watch.go:305,446,528`).
4. Verify: local buffer `<tmp>/buffer.jsonl` has redacted+signed events; then query the target DB
   `raw_events` / `sessions` / `timeline_events` for the session id.

Note: a CLI-side-only e2e (redaction → signing → local buffer) works **today** with no backend and
proves the on-device half. The DB half is blocked on §0 + §3.

---

## §5. On-device e2e result (CLI-only, no backend) — PASSED

Ran `promptster-teams watch` in a fully sandboxed env (`CLAUDE_CONFIG_DIR`, `PROMPTSTER_STATE_DIR`,
`PROMPTSTER_BUFFER_PATH`, `PROMPTSTER_TEAMS_WATCH_DIR` all redirected to a temp dir; ingest URL
pointed at a dead port so only the local buffer path exercised). Fed a 5-line synthetic Claude
transcript (prompt → assistant text+usage → Bash tool_use → tool_result → prompt) with four secrets
planted in prompt text, assistant text, and a shell command.

**Result: 4 events captured → redacted → normalized → signed → chained → buffered.**

- **Redaction (both layers):** all four planted secrets stripped, in both `data` and `rawPayload`:
  - `ghp_…` GitHub PAT → `[REDACTED:np.github.1]` (Titus rule)
  - `AKIA…` AWS key → `[REDACTED_AWS_KEY]` (supplemental)
  - `sk-proj-…` LLM key → `[REDACTED_LLM_KEY]` (supplemental)
  - `API_KEY=…` → `API_KEY=[REDACTED]` (supplemental)
  - Zero leaks: grep for each raw secret in the buffer returned nothing.
- **Signing/chain:** genesis event + 3 chained; each `prevSig` == prior event's `sig`.
- **Cryptographic verification:** all 4 `sig` values verified with `ed25519.Verify` against the
  device pubkey using the CLI's own `buildSigningMessage` (canonical, TS-mirrored). Chain intact.
- **Envelope:** well-formed — `kind`/`source=claude-code`/`actor`/`provenance` populated; per-event
  token usage and model preserved; `sessionId` = device id (`dev-…`), transcript session retained
  at `data.meta.ideSessionId` (by design).
- **Ingest POSTs failed** (dead URL, as intended) and buffering still happened — confirms the
  buffer-then-send ordering, so the local audit trail is independent of backend reachability.

**Conclusion:** the on-device half (tail → redact → normalize → sign → chain → local buffer) works
and is verifiable today.

---

## §6. Full e2e result (CLI → teams ingest → teams DB) — PASSED

Implemented option (A) and ran the real end-to-end. Backend changes live on
`promptster-backend` branch **`teams-ingest`** (worktree `.claude/worktrees/teams-ingest`,
uncommitted): new `POST /v1/teams/ingest`, `requireTeamsAuth` (X-API-Key → `org_api_keys` in the
teams DB), `app.teamsDb = createDb(TEAMS_DATABASE_URL)`, shared helpers extracted to
`routes/hooks-shared.ts`, device-pubkey pinning, hiring queue jobs dropped. Backend suite: **358
tests pass** incl. 5 new teams-hooks tests; tsc + oxlint clean.

**Run:** started the api locally (`pnpm --filter @promptster/api dev`, :3001) pointed at the real
teams DB. Seeded org `team-e2e-org` + an org API key (`psk_live_…`, sha256 → `org_api_keys.key_hash`)
in the teams DB. Drove the **actual CLI** (`promptster-teams watch`, sandboxed) with
`PROMPTSTER_TEAMS_API_URL=http://127.0.0.1:3001`,
`PROMPTSTER_TEAMS_INGEST_PATH=/v1/teams/ingest`, `PROMPTSTER_TEAMS_TOKEN=psk_live_…` against the same
5-line synthetic transcript (4 planted secrets). CLI reported `sent 4 events`.

**Verified in the teams DB (`eeqtxycyglfyqzfwwuli`), all under `org_id='team-e2e-org'`:**

| Check | Result |
|---|---|
| `sessions` | 1 row (`dev-eaadff93e23fe6d4`), `source_service={claude-code}` |
| `raw_events` | 4 rows, **`sig_verified=true` on all 4**, prevSig chain intact (genesis + 3) |
| `timeline_events` | 4 rows |
| Org scoping (F2 fixed) | token auth resolved `org_id`; every row scoped to the org |
| Device pubkey (F3 fixed) | `sessions.signing_pubkey` pinned; signatures now verified server-side |
| Correct DB (F1 fixed) | rows in teams DB; main DB untouched |
| Server-side redaction | 0 leaks for all 4 planted secrets in stored `payload`; redaction markers present |
| Transcript session | preserved at `payload.data.meta.ideSessionId` |

**Leftover test data** in the teams DB (namespaced, easy to purge): org `team-e2e-org`, one
`org_api_keys` row, 1 session / 4 raw_events / 4 timeline_events. Purge with
`delete from sessions where org_id='team-e2e-org'; delete from org_api_keys where org_id='team-e2e-org'; delete from organizations where id='team-e2e-org';`
(raw_events/timeline cascade-or-delete by org_id first).

### Remaining follow-ups (not blockers)
- [ ] Commit / PR backend branch `teams-ingest` (currently uncommitted in the worktree).
- [ ] CLI: default `ingestEndpoint()` to `/v1/teams/ingest` (today defaults to `/v1/hooks/ingest`,
      overridden via `PROMPTSTER_TEAMS_INGEST_PATH` for this run).
- [ ] CLI `doctor`: ping the endpoint + surface auth failures.
- [ ] Decide whether teams orgs/keys are minted in the teams DB via a UI/script (today seeded by hand).
- ~~Hiring `/v1/hooks/ingest` has the *same* unpinned-pubkey gap (F3)~~ — **CORRECTION: it does
      not.** The hiring CLI registers its pubkey via the `redeem` body (`promptster-cli/api.go:92`
      → `candidate.ts:225`), so hiring sessions are pinned at creation and `/v1/hooks/ingest`
      verifies against them (`sig_verified=true`). Hiring and teams pin differently because they
      enroll differently (redeem-body vs device-header); both are correct. The ed25519 chain is
      actively consumed by `promptster verify` (`cmd_verify.go`) via `/v1/sessions/:id/signed-log`
      (`sessions.ts:390`) and is a stated product differentiator — keep it, don't rip it out. No
      change made to the hiring path (header-pinning there would be dead code: the hiring CLI never
      sends the device-pubkey header).
