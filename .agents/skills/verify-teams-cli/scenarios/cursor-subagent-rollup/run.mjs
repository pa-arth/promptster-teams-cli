#!/usr/bin/env node
// Replays REAL Cursor subagent capture through the sandboxed binary and asserts
// the subagent's hook events roll up to the parent with no duplicate copy.
//
// Fixtures were captured 2026-09-28 from cursor-agent 2026.09.23-86fc751
// (a Task subagent running two shell commands), then placed in the EDITOR's
// layout — <parent>/subagents/<child>.jsonl — with transcript_path null, which
// is what the editor sends for a subagent (it looks the path up by child id
// alone). Headless cursor-agent runs subagents as top-level conversations and
// is not this case.
//
// Usage (after `node $CT build`):  node scenarios/cursor-subagent-rollup/run.mjs
// Exit 0 = pass. Prints one JSON verdict. Fails on origin/main before #253.
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const CT = path.resolve(HERE, "..", "..", "control-teams-cli.mjs");
const ROOT = process.env.PROMPTSTER_VERIFY_ROOT || path.join(os.homedir(), ".promptster-verify", "teams-cli");
const HOME = path.join(ROOT, "sandbox", "home");
const STATE = path.join(HOME, ".promptster-teams");
const PARENT = "668e1dd6-6f4b-484c-91fa-5240841f3df6";
const CLAIMED = "585adb63-4618-47a4-a145-3b2deb7d8ad1"; // gets hook events
const CONTROL = "585adb63-4618-47a4-a145-3b2deb7d8ad2"; // transcript only

const ct = (args, env = {}) => {
  const r = spawnSync("node", [CT, ...args], { encoding: "utf8", env: { ...process.env, ...env } });
  return JSON.parse(r.stdout);
};

ct(["cleanup"]);
const login = ct(["run", "login", "--key", "PSE-ABCD-EFGH-JKLM-NPQR-STUV-WXYZ"]);

const work = path.join(HOME, "probe");
const tdir = path.join(HOME, ".cursor", "projects", "sbx-probe", "agent-transcripts", PARENT);
fs.mkdirSync(path.join(tdir, "subagents"), { recursive: true });
fs.mkdirSync(work, { recursive: true });
const fix = (s) => s.replaceAll("__WORK__", work);
fs.writeFileSync(path.join(tdir, `${PARENT}.jsonl`), fix(fs.readFileSync(path.join(HERE, "parent.jsonl"), "utf8")));
const claimedPath = path.join(tdir, "subagents", `${CLAIMED}.jsonl`);
fs.writeFileSync(claimedPath, ""); // Cursor creates the child file when the subagent starts

const hookRuns = [];
for (const f of ["afterShellExecution-1.json", "afterShellExecution-2.json"]) {
  const p = JSON.parse(fix(fs.readFileSync(path.join(HERE, f), "utf8")));
  Object.assign(p, { conversation_id: CLAIMED, session_id: CLAIMED, generation_id: CLAIMED, transcript_path: null });
  const tmp = path.join(os.tmpdir(), `sub-hook-${process.pid}-${f}`);
  fs.writeFileSync(tmp, JSON.stringify(p));
  hookRuns.push(ct(["run", "cursor-hook"], { PROMPTSTER_VERIFY_STDIN: tmp }).evidence);
  fs.rmSync(tmp);
}

const child = fix(fs.readFileSync(path.join(HERE, "child.jsonl"), "utf8"));
fs.writeFileSync(claimedPath, child);
fs.writeFileSync(path.join(tdir, "subagents", `${CONTROL}.jsonl`), child.replaceAll("probe.txt", "probe2.txt"));

const commands = () => {
  const seen = new Map();
  for (const f of ["buffer.jsonl", "outbox.jsonl"]) {
    const p = path.join(STATE, f);
    if (!fs.existsSync(p)) continue;
    for (const line of fs.readFileSync(p, "utf8").split("\n")) {
      let e; try { e = JSON.parse(line); } catch { continue; }
      e = e.event ?? e;
      if (e?.source === "cursor" && e.kind === "command") seen.set(e.id, e);
    }
  }
  return [...seen.values()].map((e) => ({
    session: e.sessionId, agentId: e.data?.agentId ?? null,
    rail: e.provenance?.methods?.[0], command: e.data?.command,
  }));
};

// The control transcript proves the watcher polled; wait for it.
const deadline = Date.now() + 120_000;
let got = [];
while (Date.now() < deadline) {
  got = commands();
  if (got.some((c) => c.agentId === CONTROL)) break;
  spawnSync("sleep", ["2"]);
}
const claimsFile = path.join(STATE, "cursor-hook-claims.json");
const claims = fs.existsSync(claimsFile) ? Object.keys(JSON.parse(fs.readFileSync(claimsFile, "utf8")).claims) : [];

const checks = {
  watcherPolled: got.some((c) => c.agentId === CONTROL && c.session === PARENT),
  subagentHookEventsUnderParent: got.filter((c) => c.rail === "cursor-hook").length === 2 &&
    got.filter((c) => c.rail === "cursor-hook").every((c) => c.session === PARENT && c.agentId === CLAIMED),
  noOrphanSession: !got.some((c) => c.session === CLAIMED),
  subagentTranscriptClaimed: claims.some((k) => k.endsWith(`subagents/${CLAIMED}.jsonl`)),
  noDuplicateFromTranscript: !got.some((c) => c.agentId === CLAIMED && c.rail === "transcript-jsonl"),
};
const ok = Object.values(checks).every(Boolean);
console.log(JSON.stringify({ ok, checks, commands: got, claims, evidence: [login.evidence, ...hookRuns] }, null, 1));
ct(["cleanup"]);
process.exit(ok ? 0 : 1);
