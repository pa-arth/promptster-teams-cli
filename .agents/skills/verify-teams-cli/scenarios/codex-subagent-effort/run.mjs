#!/usr/bin/env node
// Drives the real codex watcher over a parent rollout plus one delegated
// (subagent) rollout and asserts the delegate's reasoning tier survives
// normalize -> on-device projection -> buffer.
//
// Line shapes are real Codex rollouts: session_meta/token_count/agent_message
// from the 0.146 capture the normalize tests pin, turn_context from a local
// gpt-6-astra delegate (2026-09-26), trimmed. Timestamps are "now" because the
// watcher only tails rollouts that started after it did.
//
// Usage (after `node $CT build`):  node scenarios/codex-subagent-effort/run.mjs
// Exit 0 = pass. Fails before teams-cli stamps effort on subagent_usage.
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const CT = path.resolve(HERE, "..", "..", "control-teams-cli.mjs");
// Own root: this scenario cleans up on start and finish.
process.env.PROMPTSTER_VERIFY_ROOT ||= path.join(os.homedir(), ".promptster-verify", "teams-cli-codex-subagent-effort");
const SBX = path.join(process.env.PROMPTSTER_VERIFY_ROOT, "sandbox");
const HOME = path.join(SBX, "home");
const STATE = path.join(HOME, ".promptster-teams");
const PARENT = "019fb396-91d9-7770-bcc0-329fcedfa8e0";
const CHILD = "019fb396-9280-7710-91d4-f36e7797376b";

const ct = (args) => JSON.parse(spawnSync("node", [CT, ...args], { encoding: "utf8", env: process.env }).stdout);
const sleep = (s) => spawnSync("sleep", [String(s)]);

ct(["cleanup"]);
const login = ct(["run", "login", "--key", "PSE-ABCD-EFGH-JKLM-NPQR-STUV-WXYZ"]);
const hb = path.join(STATE, "codex-watcher.json");
for (const until = Date.now() + 60_000; Date.now() < until; sleep(1)) {
  try { if (JSON.parse(fs.readFileSync(hb, "utf8")).lastHeartbeat) break; } catch {}
}
sleep(1);

const t = (s) => new Date(Date.now() + s * 1000).toISOString();
const turnContext = (s, settings) =>
  `{"timestamp":"${t(s)}","type":"turn_context","payload":{"model":"gpt-6-astra","effort":null,"collaboration_mode":{"settings":{"model":"gpt-6-astra",${settings}}}}}`;
const tokens = (s, i, o) =>
  `{"timestamp":"${t(s)}","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":${i},"cached_input_tokens":0,"output_tokens":${o},"total_tokens":${i + o}}}}}`;
const final = (s, m) => `{"timestamp":"${t(s)}","type":"event_msg","payload":{"type":"agent_message","message":"${m}","phase":"final_answer"}}`;

const parent = [
  `{"timestamp":"${t(0)}","type":"session_meta","payload":{"session_id":"${PARENT}","id":"${PARENT}","timestamp":"${t(0)}","cwd":"${HOME}","originator":"codex-tui","cli_version":"0.146.0","source":"cli","thread_source":"user","model_provider":"openai"}}`,
  turnContext(1, `"reasoning_effort":"high"`),
  `{"timestamp":"${t(2)}","type":"event_msg","payload":{"type":"user_message","message":"fix the retry backoff","images":[]}}`,
  tokens(3, 1000, 50),
  final(4, "Delegated."),
];
const child = [
  `{"timestamp":"${t(1)}","type":"session_meta","payload":{"session_id":"${PARENT}","id":"${CHILD}","parent_thread_id":"${PARENT}","timestamp":"${t(1)}","cwd":"${HOME}","originator":"codex-tui","source":{"subagent":{"other":"guardian"}},"thread_source":"subagent","model_provider":"openai"}}`,
  turnContext(2, `"reasoning_effort":"low"`),
  tokens(3, 17718, 237),
  final(4, "allow"),
];
const day = new Date().toISOString().slice(0, 10).split("-");
const dir = path.join(SBX, "codexhome", "sessions", ...day);
fs.mkdirSync(dir, { recursive: true });
const stamp = new Date().toISOString().slice(0, 19).replaceAll(":", "-");
fs.writeFileSync(path.join(dir, `rollout-${stamp}-${PARENT}.jsonl`), parent.join("\n") + "\n");
fs.writeFileSync(path.join(dir, `rollout-${stamp}-${CHILD}.jsonl`), child.join("\n") + "\n");

const usage = () => {
  const seen = new Map();
  const p = path.join(STATE, "buffer.jsonl");
  if (!fs.existsSync(p)) return [];
  for (const line of fs.readFileSync(p, "utf8").split("\n")) {
    let e; try { e = JSON.parse(line); } catch { continue; }
    if (e?.source === "codex" && (e.kind === "ai_response" || e.kind === "subagent_usage")) seen.set(e.id, e);
  }
  return [...seen.values()].map((e) => ({ kind: e.kind, session: e.sessionId, agentId: e.data?.agentId ?? null, effort: e.data?.effort ?? null }));
};
let got = [];
for (const until = Date.now() + 120_000; Date.now() < until; sleep(2)) {
  got = usage();
  if (got.some((e) => e.kind === "subagent_usage") && got.some((e) => e.kind === "ai_response")) break;
}

const sub = got.filter((e) => e.kind === "subagent_usage");
const checks = {
  mainThreadEffort: got.some((e) => e.kind === "ai_response" && e.effort === "high"),
  delegateCaptured: sub.length > 0 && sub.every((e) => e.session === PARENT && e.agentId === CHILD),
  delegateEffortSurvivesProjection: sub.length > 0 && sub.every((e) => e.effort === "low"),
};
const ok = Object.values(checks).every(Boolean);
console.log(JSON.stringify({ ok, checks, events: got, evidence: login.evidence }, null, 1));
if (!process.env.PROMPTSTER_VERIFY_KEEP) ct(["cleanup"]);
process.exit(ok ? 0 : 1);
