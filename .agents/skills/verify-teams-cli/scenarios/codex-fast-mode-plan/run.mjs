#!/usr/bin/env node
// Drives the real codex watcher over one rollout that switches into fast mode
// and carries a plan on its rate limits, then asserts what reached the buffer:
// codex_session_usage has serviceTier + fast-mode counters (only the growth
// while fast was on), and windowUsage has planType.
//
// Line shapes follow real Codex 0.155 rollouts: thread_settings_applied carries
// thread_settings.service_tier ("priority" = fast mode); token_count carries
// rate_limits.plan_type beside the weekly window.
//
// Usage (after `node $CT build`):  node scenarios/codex-fast-mode-plan/run.mjs
// Exit 0 = pass. The window scan runs every 60s, so this takes ~1-2 minutes.
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const CT = path.resolve(HERE, "..", "..", "control-teams-cli.mjs");
process.env.PROMPTSTER_VERIFY_ROOT ||= path.join(os.homedir(), ".promptster-verify", "teams-cli-codex-fast-mode-plan");
const SBX = path.join(process.env.PROMPTSTER_VERIFY_ROOT, "sandbox");
const HOME = path.join(SBX, "home");
const STATE = path.join(HOME, ".promptster-teams");
const THREAD = "019fb396-91d9-7770-bcc0-329fcedfa8e1";

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
const settings = (s, tier) =>
  `{"timestamp":"${t(s)}","type":"event_msg","payload":{"type":"thread_settings_applied","thread_id":"${THREAD}","thread_settings":{"model":"gpt-6-astra","model_provider_id":"openai","service_tier":"${tier}","approval_policy":"never"}}}`;
const resets = Math.floor(Date.now() / 1000) + 6 * 86400;
const tokens = (s, i, c, o) =>
  `{"timestamp":"${t(s)}","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":${i},"cached_input_tokens":${c},"output_tokens":${o},"total_tokens":${i + o}}},"rate_limits":{"limit_id":"codex","primary":{"used_percent":12.0,"window_minutes":10080,"resets_at":${resets}},"secondary":null,"plan_type":"prolite"}}}`;
const rollout = [
  `{"timestamp":"${t(0)}","type":"session_meta","payload":{"session_id":"${THREAD}","id":"${THREAD}","timestamp":"${t(0)}","cwd":"${HOME}","originator":"codex-tui","cli_version":"0.155.0","source":"cli","thread_source":"user","model_provider":"openai"}}`,
  `{"timestamp":"${t(1)}","type":"turn_context","payload":{"model":"gpt-6-astra"}}`,
  settings(1, "default"),
  tokens(2, 1000, 500, 50),
  settings(3, "priority"),
  tokens(4, 3000, 2000, 150),
];
const day = new Date().toISOString().slice(0, 10).split("-");
const dir = path.join(SBX, "codexhome", "sessions", ...day);
fs.mkdirSync(dir, { recursive: true });
const stamp = new Date().toISOString().slice(0, 19).replaceAll(":", "-");
fs.writeFileSync(path.join(dir, `rollout-${stamp}-${THREAD}.jsonl`), rollout.join("\n") + "\n");

const events = () => {
  const p = path.join(STATE, "buffer.jsonl");
  if (!fs.existsSync(p)) return [];
  const seen = new Map();
  for (const line of fs.readFileSync(p, "utf8").split("\n")) {
    let e; try { e = JSON.parse(line); } catch { continue; }
    if (e?.kind === "codex_session_usage" || (e?.kind === "windowUsage" && e.data?.provider === "codex")) seen.set(e.id, e);
  }
  return [...seen.values()].map((e) => ({ kind: e.kind, data: e.data }));
};
let got = [];
for (const until = Date.now() + 180_000; Date.now() < until; sleep(3)) {
  got = events();
  if (got.some((e) => e.kind === "windowUsage") && got.filter((e) => e.kind === "codex_session_usage").length >= 2) break;
}

const usage = got.filter((e) => e.kind === "codex_session_usage").map((e) => e.data);
const last = usage.at(-1) ?? {};
const win = got.find((e) => e.kind === "windowUsage")?.data ?? {};
const checks = {
  standardTurnHasNoFastTokens: usage.some((d) => d.serviceTier === "default" && d.fastInputTokens === 0),
  fastTierReported: last.serviceTier === "fast",
  // Only the growth after switching to priority counts: 3000-1000, 2000-500, 150-50.
  fastCountersAreTheFastDelta: last.fastInputTokens === 2000 && last.fastCacheReadTokens === 1500 && last.fastOutputTokens === 100,
  totalsUnchanged: last.inputTokens === 3000 && last.cacheReadTokens === 2000 && last.outputTokens === 150,
  planTypeOnWindowUsage: win.planType === "prolite" && win.weeklyPct === 12,
};
const ok = Object.values(checks).every(Boolean);
console.log(JSON.stringify({ ok, checks, usage, windowUsage: win, evidence: login.evidence }, null, 1));
if (!process.env.PROMPTSTER_VERIFY_KEEP) ct(["cleanup"]);
process.exit(ok ? 0 : 1);
