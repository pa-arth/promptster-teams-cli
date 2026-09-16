#!/usr/bin/env node
// herdr-run — run ONE prompt through an interactive agent (codex, cursor, …)
// in a throwaway herdr tab, print everything the agent's terminal showed, and
// tear the tab down.
//
//   node herdr-run.mjs <kind> <cwd> <prompt-file> [--keep] [--timeout-ms N]
//
// Why this exists: `codex exec` and `cursor-agent -p` are separate,
// non-interactive entry points. herdr drives the SAME interactive binaries the
// human uses, already signed in, so an eval scores the agent as it actually
// ships rather than a headless side door.
//
// Exit 0 with the transcript on stdout. Exit non-zero with {ok:false,...} on
// stdout if the agent never started, never ran, or never settled.

import { spawnSync } from "node:child_process";
import fs from "node:fs";

const [kind, cwd, promptFile, ...rest] = process.argv.slice(2);
const KEEP = rest.includes("--keep");
const ti = rest.indexOf("--timeout-ms");
const TIMEOUT = ti > -1 ? Number(rest[ti + 1]) : 900_000;

const die = (error, hint, extra = {}) => {
  process.stdout.write(JSON.stringify({ ok: false, error, hint, ...extra }, null, 2) + "\n");
  process.exit(1);
};

if (!kind || !cwd || !promptFile) {
  die("usage: herdr-run.mjs <kind> <cwd> <prompt-file>", "kind is a herdr agent kind, e.g. codex or cursor.");
}
if (!fs.existsSync(promptFile)) die(`prompt file not found: ${promptFile}`, "Write the prompt to a file first.");
const prompt = fs.readFileSync(promptFile, "utf8");

const herdr = (args) => {
  const r = spawnSync("herdr", args, { encoding: "utf8", maxBuffer: 64 * 1024 * 1024 });
  return { code: r.status, out: (r.stdout || "") + (r.stderr || "") };
};
const json = (args) => {
  const { out } = herdr(args);
  try { return JSON.parse(out); } catch { return null; }
};

const name = `eval-${kind}-${Date.now().toString(36)}`;

// Interactive agents stop at a permission dialog and wait forever. An eval that
// does not pass the bypass flag scores "no answer" for a run where the agent was
// simply waiting to be allowed to start — cursor sat at
// "Not in allowlist: node — Run this command?" for every case.
const AGENT_ARGS = {
  cursor: ["--force"],
  codex: ["--dangerously-bypass-approvals-and-sandbox"],
};
const agentArgs = AGENT_ARGS[kind] ?? [];

// ---- create a tab of our own. Never reuse one: a tab already running the
// human's work would get our prompt injected into their session.
const made = json(["tab", "create", "--cwd", cwd, "--label", name, "--no-focus"]);
const pane = made?.result?.root_pane?.pane_id;
const tab = made?.result?.tab?.tab_id;
if (!pane) die("could not create a herdr tab", "Is the herdr server running? `herdr status`.", { raw: made });

const cleanup = () => { if (!KEEP && tab) herdr(["tab", "close", tab]); };
process.on("exit", cleanup);
for (const s of ["SIGINT", "SIGTERM"]) process.on(s, () => { cleanup(); process.exit(130); });

// ---- start the agent.
// `tab create` returns before the new pane's shell is at a prompt, so starting
// immediately loses a race and herdr answers `agent_pane_busy: not an available
// shell`. That failed in under a second and looked exactly like the agent
// refusing to answer, which is the worst kind of harness bug: it reports a real
// number for a run that never happened. Retry until the shell is actually there.
const sleepSync = (ms) => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
let started = null;
let lastErr = null;
for (let attempt = 0; attempt < 15; attempt++) {
  started = json(["agent", "start", name, "--kind", kind, "--pane", pane, "--timeout", "120000", ...(agentArgs.length ? ["--", ...agentArgs] : [])]);
  if (started?.result?.agent?.interactive_ready) break;
  lastErr = started?.error ?? started?.raw?.error ?? started;
  const code = started?.error?.code ?? started?.raw?.error?.code ?? "";
  if (code && code !== "agent_pane_busy") break; // a real failure, not the race
  sleepSync(2000);
}
if (!started?.result?.agent?.interactive_ready) {
  die(
    `${kind} did not become interactive`,
    `herdr could not bring ${kind} to a prompt in that pane. If the error is agent_pane_busy the shell never came up; if ${kind} is asking to sign in, log in once by hand (herdr agent attach ${name}) — an eval cannot authenticate for you.`,
    { lastError: lastErr }
  );
}

const status = () => json(["agent", "get", name])?.result?.agent?.agent_status ?? "unknown";
const screen = () => herdr(["agent", "read", name, "--source", "recent-unwrapped", "--lines", "4000", "--format", "text"]).out;

// A signed-out agent sits "idle" at a login prompt forever, which is
// indistinguishable from "finished" unless you look at what it is showing.
const early = screen();
if (/Signing in with the browser|click this link to log in|Please run .*login|not authenticated/i.test(early)) {
  die(`${kind} is not signed in`, `Its terminal is showing a login prompt, so it can never answer. Sign in once (herdr agent attach ${name}), then re-run. Leaving the tab open for you.`, { keptTab: tab });
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const t0 = Date.now();

// ---- wait for it to pick the prompt up. `agent prompt` returns before the
// agent transitions, so waiting on "not working" immediately matches the idle
// it was already in and returns instantly with an empty answer.
herdr(["agent", "prompt", name, prompt]);

let sawWorking = false;
for (let i = 0; i < 20 && !sawWorking; i++) {
  if (status() === "working") { sawWorking = true; break; }
  await sleep(1500);
}

// Some TUIs (cursor-agent) take the prompt into the composer but do not submit
// it, so the agent sits idle with the question typed and never answers. One
// Enter commits it. Harmless where the agent already started working.
if (!sawWorking) {
  herdr(["agent", "send-keys", name, "Enter"]);
  for (let i = 0; i < 20 && !sawWorking; i++) {
    if (status() === "working") { sawWorking = true; break; }
    await sleep(1500);
  }
}

// ---- settle on STATUS, not on the screen. These TUIs animate a background
// (codex renders drifting braille dots), so the rendered text never compares
// equal twice and a "screen stopped changing" check waits forever. Require the
// non-working status to hold across consecutive polls so a momentary idle
// between tool calls does not read as finished.
let stableFor = 0;
while (Date.now() - t0 < TIMEOUT) {
  await sleep(3000);
  const st = status();
  if (st === "done" || st === "idle" || st === "blocked") {
    if (++stableFor >= 3) break;
  } else {
    stableFor = 0;
  }
}

const transcript = screen();
const elapsedMs = Date.now() - t0;
if (Date.now() - t0 >= TIMEOUT) {
  process.stdout.write(transcript + `\n[herdr-run] TIMED OUT after ${Math.round(elapsedMs / 1000)}s; transcript above is whatever it had produced.\n`);
  process.exit(1);
}

process.stdout.write(transcript + `\n[herdr-run] agent=${kind} elapsedMs=${elapsedMs} sawWorking=${sawWorking}\n`);
