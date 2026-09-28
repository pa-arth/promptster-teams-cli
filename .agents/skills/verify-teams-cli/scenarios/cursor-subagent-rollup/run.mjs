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
// Its OWN root by default: this scenario runs `cleanup` on start and finish, and
// on the shared root that would kill another run's daemon and delete its sandbox.
process.env.PROMPTSTER_VERIFY_ROOT ||= path.join(os.homedir(), ".promptster-verify", "teams-cli-cursor-subagent-rollup");
const ROOT = process.env.PROMPTSTER_VERIFY_ROOT;
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

// Wait for the cursor watcher's first heartbeat. A transcript already on disk
// when it starts is seeded to EOF as history, so writing fixtures before it is up
// makes the control transcript invisible and the run a false failure.
const hb = path.join(STATE, "cursor-watcher.json");
for (const until = Date.now() + 60_000; Date.now() < until; spawnSync("sleep", ["1"])) {
  try { if (JSON.parse(fs.readFileSync(hb, "utf8")).lastHeartbeat) break; } catch {}
}

const work = path.join(HOME, "probe");
const tdir = path.join(HOME, ".cursor", "projects", "sbx-probe", "agent-transcripts", PARENT);
fs.mkdirSync(path.join(tdir, "subagents"), { recursive: true });
fs.mkdirSync(work, { recursive: true });
const fix = (s) => s.replaceAll("__WORK__", work);
fs.writeFileSync(path.join(tdir, `${PARENT}.jsonl`), fix(fs.readFileSync(path.join(HERE, "parent.jsonl"), "utf8")));
const claimedPath = path.join(tdir, "subagents", `${CLAIMED}.jsonl`);
fs.writeFileSync(claimedPath, ""); // Cursor creates the child file when the subagent starts

const hookRuns = [];
const hook = (payload, name) => {
  const tmp = path.join(os.tmpdir(), `sub-hook-${process.pid}-${name}`);
  fs.writeFileSync(tmp, JSON.stringify(payload));
  hookRuns.push(ct(["run", "cursor-hook"], { PROMPTSTER_VERIFY_STDIN: tmp }).evidence);
  fs.rmSync(tmp);
};
// The parent's own turn fires first and, like every main-chain hook in the
// editor, names its transcript — which is what claims the parent session.
const main = JSON.parse(fix(fs.readFileSync(path.join(HERE, "afterShellExecution-1.json"), "utf8")));
Object.assign(main, { conversation_id: PARENT, session_id: PARENT, generation_id: PARENT,
  transcript_path: path.join(tdir, `${PARENT}.jsonl`), command: "echo main-chain" });
hook(main, "main");
for (const f of ["afterShellExecution-1.json", "afterShellExecution-2.json"]) {
  const p = JSON.parse(fix(fs.readFileSync(path.join(HERE, f), "utf8")));
  Object.assign(p, { conversation_id: CLAIMED, session_id: CLAIMED, generation_id: CLAIMED, transcript_path: null });
  hook(p, f);
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
  mainChainUnchanged: got.some((c) => c.command === "echo main-chain" && c.session === PARENT && c.agentId === null),
  subagentHookEventsUnderParent: got.filter((c) => c.rail === "cursor-hook" && c.command !== "echo main-chain").length === 2 &&
    got.filter((c) => c.rail === "cursor-hook" && c.command !== "echo main-chain").every((c) => c.session === PARENT && c.agentId === CLAIMED),
  noOrphanSession: !got.some((c) => c.session === CLAIMED),
  subagentTranscriptClaimed: claims.some((k) => k.endsWith(`subagents/${CLAIMED}.jsonl`)),
  noDuplicateFromTranscript: !got.some((c) => c.agentId === CLAIMED && c.rail === "transcript-jsonl"),
};
const ok = Object.values(checks).every(Boolean);
console.log(JSON.stringify({ ok, checks, commands: got, claims, evidence: [login.evidence, ...hookRuns] }, null, 1));
if (!process.env.PROMPTSTER_VERIFY_KEEP) ct(["cleanup"]); // KEEP=1 leaves the sandbox for `inspect`
process.exit(ok ? 0 : 1);
