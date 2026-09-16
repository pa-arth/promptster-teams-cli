#!/usr/bin/env node
// control-teams-cli — drive the real promptster-teams Go CLI inside a sandbox
// that cannot touch the engineer's own config, credentials, launchd jobs, or
// the hosted ingest API.
//
// Every command prints JSON. Every failure prints {ok:false, error, hint}.
//
//   node control-teams-cli.mjs help
//
// Why a sandbox and not just env vars: PROMPTSTER_STATE_DIR redirects most
// capture state, but NOT ~/.promptster-teams/credentials, NOT ~/Library/
// LaunchAgents, NOT ~/.claude, NOT ~/.cursor — those all resolve through
// os.UserHomeDir(). HOME is the master seam. And `launchctl` is keyed on UID,
// not HOME, so it escapes the sandbox entirely; we shim it on PATH.

import fs from "node:fs";
import path from "node:path";
import os from "node:os";
import { spawnSync, execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const REPO = path.resolve(HERE, "..", "..", "..");
const BIN = path.join(HERE, "bin", "promptster-teams");

const ROOT = process.env.PROMPTSTER_VERIFY_ROOT || path.join(os.homedir(), ".promptster-verify", "teams-cli");
const SBX = path.join(ROOT, "sandbox");
const EVIDENCE = process.env.PROMPTSTER_VERIFY_EVIDENCE || path.join(ROOT, "evidence");
const PROCS = path.join(ROOT, "procs.json");

// An address nothing listens on. The CLI buffers locally when ingest is
// unreachable, so this is the whole "no real API" guarantee.
const DEAD_API = "http://127.0.0.1:9";

// Commands the CLI shells out to that are keyed on the UID or the machine, not
// on HOME — they would escape the sandbox. Shimmed to a no-op that logs.
const ESCAPING_BINARIES = ["launchctl", "systemctl", "schtasks"];

const out = (o) => { process.stdout.write(JSON.stringify(o, null, 2) + "\n"); return o.ok === false ? 1 : 0; };
const fail = (error, hint, extra = {}) => out({ ok: false, error, hint, ...extra });
const ts = () => new Date().toISOString().replace(/[:.]/g, "-");

// ----------------------------------------------------------------- sandbox

const sbxHome = () => path.join(SBX, "home");
const sbxState = () => path.join(sbxHome(), ".promptster-teams");
const shimLog = () => path.join(SBX, "escaped-commands.log");

function makeSandbox({ fresh = false } = {}) {
  if (fresh && fs.existsSync(SBX)) fs.rmSync(SBX, { recursive: true, force: true });
  for (const d of [sbxHome(), sbxState(), path.join(SBX, "bin"), path.join(SBX, "codexhome"), EVIDENCE])
    fs.mkdirSync(d, { recursive: true });
  for (const name of ESCAPING_BINARIES) {
    const p = path.join(SBX, "bin", name);
    fs.writeFileSync(p, `#!/bin/sh\nprintf '%s %s\\n' "${name}" "$*" >> ${JSON.stringify(shimLog())}\nexit 0\n`);
    fs.chmodSync(p, 0o755);
  }
  if (!fs.existsSync(shimLog())) fs.writeFileSync(shimLog(), "");
  return SBX;
}

// The env every sandboxed run gets. `env -i`-style: we build it from nothing so
// a PROMPTSTER_TEAMS_TOKEN sitting in the agent's own shell cannot leak in and
// make an unauthenticated sandbox look logged in.
function sandboxEnv(extra = {}) {
  return {
    PATH: `${path.join(SBX, "bin")}:/usr/bin:/bin:/usr/sbin:/sbin`,
    HOME: sbxHome(),
    TMPDIR: process.env.TMPDIR || "/tmp",
    TERM: "dumb",
    PROMPTSTER_STATE_DIR: sbxState(),
    PROMPTSTER_BUFFER_PATH: path.join(sbxState(), "buffer.jsonl"),
    PROMPTSTER_TEAMS_API_URL: DEAD_API,
    PROMPTSTER_API_URL: DEAD_API,
    CODEX_HOME: path.join(SBX, "codexhome"),
    CLAUDE_CONFIG_DIR: path.join(sbxHome(), ".claude"),
    PROMPTSTER_CURSOR_HOME: path.join(sbxHome(), ".cursor"),
    // doctor/status do a best-effort GitHub release probe unless this is set.
    PROMPTSTER_TEAMS_NO_AUTO_UPDATE: "1",
    ...extra,
  };
}

// ----------------------------------------------------------------- processes

const readProcs = () => { try { return JSON.parse(fs.readFileSync(PROCS, "utf8")); } catch { return []; } };
const writeProcs = (p) => { fs.mkdirSync(ROOT, { recursive: true }); fs.writeFileSync(PROCS, JSON.stringify(p, null, 2)); };

const alive = (pid) => { try { process.kill(pid, 0); return true; } catch { return false; } };

// Only ever act on a pid whose executable is OUR build. The engineer's own
// capture daemon has the same process name; killing by name would take it out.
//
// Reads `args=`, not `comm=`. On Linux `comm=` is the basename ("promptster"),
// so an absolute-path compare NEVER matches and cleanup silently skips the
// harness's own daemon: procs.json is cleared and the sandbox removed while the
// process is still live, leaving an untracked daemon to contaminate the next
// run. `args=` carries the full argv[0] on both macOS and Linux, and we always
// exec by absolute path, so the distinction the check exists to make survives.
function isOurs(pid) {
  try {
    const argv0 = execFileSync("ps", ["-o", "args=", "-p", String(pid)], { encoding: "utf8" }).trim().split(/\s+/)[0];
    return argv0 === BIN;
  } catch { return false; }
}

// A sandboxed run may detach a daemon (login/start do). Harvest its pid from the
// pidfiles the CLI writes so cleanup can kill exactly that.
function harvestPids() {
  const found = [];
  for (const f of ["supervisor.json", "claude-watcher.json", "codex-watcher.json", "cursor-watcher.json"]) {
    try {
      const st = JSON.parse(fs.readFileSync(path.join(sbxState(), f), "utf8"));
      if (st.pid > 0) found.push({ pid: st.pid, from: f, at: new Date().toISOString() });
    } catch { /* not written */ }
  }
  if (!found.length) return [];
  const known = new Set(readProcs().map((p) => p.pid));
  const merged = readProcs().concat(found.filter((f) => !known.has(f.pid)));
  writeProcs(merged);
  return found;
}

// ----------------------------------------------------------------- build

const goFiles = () => {
  const acc = [];
  const walk = (d) => {
    for (const e of fs.readdirSync(d, { withFileTypes: true })) {
      if (e.name === ".git" || e.name === "bin" || e.name === "dist" || e.name === "node_modules") continue;
      const p = path.join(d, e.name);
      if (e.isDirectory()) walk(p);
      else if (e.name.endsWith(".go") || e.name === "go.mod" || e.name === "go.sum") acc.push(p);
    }
  };
  walk(REPO);
  return acc;
};

const newestSource = () => goFiles().reduce((m, f) => Math.max(m, fs.statSync(f).mtimeMs), 0);

function cmdBuild() {
  fs.mkdirSync(path.dirname(BIN), { recursive: true });
  let version = "dev";
  try { version = execFileSync("git", ["-C", REPO, "describe", "--tags", "--always", "--dirty"], { encoding: "utf8" }).trim(); } catch { /* keep dev */ }
  const t0 = Date.now();
  const r = spawnSync("go", [
    "build",
    `-ldflags=-s -w -X github.com/pa-arth/promptster-teams-cli/internal/version.Version=${version}`,
    "-o", BIN, "./cmd/promptster-teams",
  ], { cwd: REPO, encoding: "utf8" });
  if (r.error && r.error.code === "ENOENT")
    return fail("go toolchain not found on PATH", "Install Go (the repo's go.mod names the version) and re-run `build`.");
  if (r.status !== 0)
    return fail("go build failed", "Read compileError below and fix the source. Every drive until this passes measures the PREVIOUS build, not your change.", { compileError: (r.stderr || r.stdout || "").trim().split("\n").slice(-25) });
  return out({ ok: true, binary: BIN, version, seconds: Math.round((Date.now() - t0) / 1000) });
}

// ----------------------------------------------------------------- run

function cmdRun(args) {
  if (!args.length) return fail("run needs a subcommand", "e.g. `run doctor`, `run status --once`, `run login --key PSE-...`. Run `help` for the mapped surface.");
  if (!fs.existsSync(BIN)) return fail("no binary to run", "Run `build` first. The binary lives beside the skill and is never installed to /usr/local/bin.");
  makeSandbox();

  const runId = `${ts()}-${args[0].replace(/[^a-z0-9-]/gi, "_")}`;
  const dir = path.join(EVIDENCE, runId);
  fs.mkdirSync(dir, { recursive: true });

  const r = spawnSync(BIN, args, {
    cwd: sbxHome(),          // never the repo: `status` prints cwd as the watch scope
    env: sandboxEnv(),
    encoding: "utf8",
    input: "",               // no TTY, no stdin — interactive prompts must not hang
    timeout: Number(process.env.PROMPTSTER_VERIFY_TIMEOUT_MS || 60000),
  });

  const stdout = r.stdout ?? "";
  const stderr = r.stderr ?? "";
  fs.writeFileSync(path.join(dir, "stdout.txt"), stdout);
  fs.writeFileSync(path.join(dir, "stderr.txt"), stderr);
  const started = harvestPids();
  const meta = { argv: args, exitCode: r.status, signal: r.signal, sandbox: SBX, startedPids: started };
  fs.writeFileSync(path.join(dir, "cmd.json"), JSON.stringify(meta, null, 2));

  if (r.error && r.error.code === "ETIMEDOUT")
    return fail("the command did not exit", "It is probably a foreground command (`watch`, `status` without --once, `statusline run`). Drive `status --once` instead, or raise PROMPTSTER_VERIFY_TIMEOUT_MS.", { evidence: dir });

  return out({
    ok: true,
    ...meta,
    // exitCode is NOT the verdict for this CLI — see doctor's exitCodeIsNotAVerdict.
    stdout, stderr,
    evidence: dir,
  });
}

// ----------------------------------------------------------------- inspect

function cmdInspect(args) {
  if (!fs.existsSync(SBX)) return fail("no sandbox", "Nothing has been driven yet. Run `run doctor` (or any `run`) — it creates the sandbox.");
  const only = args[0];
  const files = [];
  const walk = (d, rel = "") => {
    for (const e of fs.readdirSync(d, { withFileTypes: true }).sort((a, b) => a.name.localeCompare(b.name))) {
      const p = path.join(d, e.name), r = rel ? `${rel}/${e.name}` : e.name;
      if (e.isDirectory()) { if (r.split("/").length < 4) walk(p, r); else files.push({ path: r + "/", dir: true }); }
      else files.push({ path: r, bytes: fs.statSync(p).size });
    }
  };
  walk(sbxHome());
  if (only) {
    const p = path.resolve(sbxHome(), only);
    // `inspect` reports sandbox contents and nothing else. Without this a value
    // containing ../ resolves outside and hands back part of an arbitrary
    // readable host file in the agent's output.
    const base = path.resolve(sbxHome());
    if (p !== base && !p.startsWith(base + path.sep))
      return fail(`path escapes the sandbox: ${only}`, "inspect only reads inside the sandbox home. Pass a path relative to it, with no leading / and no `..`.");
    if (!fs.existsSync(p)) return fail(`no such file in the sandbox: ${only}`, "Run `inspect` with no argument to list what the sandbox actually contains.", { files: files.map((f) => f.path) });
    return out({ ok: true, file: only, content: fs.readFileSync(p, "utf8").slice(0, 20000) });
  }
  const esc = fs.existsSync(shimLog()) ? fs.readFileSync(shimLog(), "utf8").trim().split("\n").filter(Boolean) : [];
  return out({
    ok: true,
    sandboxHome: sbxHome(),
    files,
    // What the CLI TRIED to do outside the sandbox. Empty is the normal case;
    // entries are evidence of the real launchd/systemd reach, safely absorbed.
    escapedCommandAttempts: esc,
    hint: "Read one with `inspect .promptster-teams/credentials`.",
  });
}

// ----------------------------------------------------------------- doctor

function cmdDoctor() {
  const checks = {}, hints = [];
  const add = (name, okv, hint) => { checks[name] = okv; if (!okv && hint) hints.push(`${name}: ${hint}`); };

  const goOK = spawnSync("go", ["version"], { encoding: "utf8" }).status === 0;
  add("goAvailable", goOK, "Install Go; `build` cannot run without it.");

  const built = fs.existsSync(BIN);
  add("binaryBuilt", built, "Run `build`.");

  let buildCurrent = false, version = null;
  if (built) {
    buildCurrent = fs.statSync(BIN).mtimeMs >= newestSource();
    const v = spawnSync(BIN, ["version"], { encoding: "utf8", env: sandboxEnv() });
    version = (v.stdout || "").trim() || null;
  }
  // The proof-looks-fine-but-measures-nothing case #1.
  add("buildCurrent", buildCurrent, "A .go file is newer than the binary — every drive is testing the PREVIOUS build. Run `build`.");

  const sandboxExists = fs.existsSync(SBX);
  // installation-id is written by doctor's own isolation probe below; it is not
  // state from a drive, so it must not make the sandbox look dirty forever.
  const stateFiles = sandboxExists && fs.existsSync(sbxState()) ? fs.readdirSync(sbxState()).filter((f) => f !== "installation-id") : [];
  add("sandboxClean", stateFiles.length === 0, `The sandbox already holds state from an earlier run (${stateFiles.length} files). Run \`cleanup\` if you need a from-scratch proof; keep it if you are inspecting a run you just did.`);

  // Proof-looks-fine case #2: isolation. Actually RUN the binary and read back
  // where it says it stores things, rather than trusting the env we set.
  let isolation = { ran: false };
  if (built) {
    makeSandbox();
    const r = spawnSync(BIN, ["doctor"], { cwd: sbxHome(), env: sandboxEnv(), encoding: "utf8", input: "", timeout: 30000 });
    const text = (r.stdout || "") + (r.stderr || "");
    const realHome = os.homedir();
    const leaks = text.split("\n").filter((l) => l.includes(realHome) && !l.includes(sbxHome()));
    isolation = {
      ran: true,
      exitCode: r.status,
      // `discover` legitimately scans /Users for other OS users; doctor must not.
      realHomeMentions: leaks.slice(0, 5),
      stateDirInsideSandbox: fs.existsSync(path.join(sbxState(), "installation-id")),
      apiHostIsDeadPort: /127\.0\.0\.1:9\b/.test(text),
    };
    add("sandboxIsolated", leaks.length === 0 && isolation.stateDirInsideSandbox,
      "The CLI reported a path under your REAL home. Anything proved from here is a statement about the engineer's machine, not about the change. Do not drive further; re-run `cleanup` then `doctor`.");
    add("ingestPointsAtDeadPort", isolation.apiHostIsDeadPort,
      "The sandbox is aimed at a reachable ingest URL. A drive could POST real captured events. Unset PROMPTSTER_TEAMS_API_URL in your shell and re-run.");
  } else {
    add("sandboxIsolated", false, "Cannot check isolation without a binary. Run `build`.");
    add("ingestPointsAtDeadPort", false, "Run `build` first.");
  }

  // Proof-looks-fine case #3: a token leaking in from the agent's own shell
  // would make an unauthenticated sandbox look logged in.
  const envToken = !!process.env.PROMPTSTER_TEAMS_TOKEN;
  add("noRealTokenInEnv", !envToken, "PROMPTSTER_TEAMS_TOKEN is set in this shell. The sandbox builds its env from scratch so it will not leak, but unset it before running `evals/run-eval.mjs`, which spawns agents that inherit your shell.");

  const authConfigured = fs.existsSync(path.join(sbxState(), "credentials"));

  // Proof-looks-fine case #4: stale daemons. State you inspect may have been
  // written by a process from an earlier run, not by the command you just drove.
  const stray = readProcs().filter((p) => alive(p.pid) && isOurs(p.pid));
  add("noStrayDaemons", stray.length === 0, `Sandbox daemon(s) from an earlier run are still alive (${stray.map((s) => s.pid).join(", ")}). They keep writing state. Run \`cleanup\`.`);

  // Proof-looks-fine case #5: not a check, a standing warning. `promptster-teams
  // doctor` prints ✗ lines and still exits 0, so "exit 0" is never a pass.
  hints.push("exitCodeIsNotAVerdict: `doctor`, `status`, `statusline status` and `autostart status` all exit 0 even when they print ✗ / !. Assert on the printed lines, never on exitCode. Only an unknown subcommand and a hard error exit 1.");

  const healthy = Object.values(checks).every(Boolean);
  return out({ ok: true, healthy, version, binary: built ? BIN : null, sandbox: SBX, evidence: EVIDENCE, authConfigured, checks, isolation, strayDaemons: stray, hints });
}

// ----------------------------------------------------------------- cleanup

function cmdCleanup() {
  const killed = [], skipped = [];
  for (const p of readProcs()) {
    if (!alive(p.pid)) { skipped.push({ ...p, why: "already exited" }); continue; }
    if (!isOurs(p.pid)) { skipped.push({ ...p, why: `pid reused by a process that is not ${BIN}` }); continue; }
    try { process.kill(p.pid, "SIGTERM"); killed.push(p.pid); } catch (e) { skipped.push({ ...p, why: String(e.message) }); }
  }
  writeProcs([]);
  const hadSandbox = fs.existsSync(SBX);
  if (hadSandbox) fs.rmSync(SBX, { recursive: true, force: true });
  const runs = fs.existsSync(EVIDENCE) ? fs.readdirSync(EVIDENCE).length : 0;
  return out({
    ok: true, killed, skipped, sandboxRemoved: hadSandbox,
    // Cleanup removes the instance, never the proof.
    evidenceKept: EVIDENCE, evidenceRuns: runs,
  });
}

// ----------------------------------------------------------------- main

const HELP = `control-teams-cli — drive the real promptster-teams Go CLI in a sandbox.

  build                 compile ./cmd/promptster-teams to ${path.relative(REPO, BIN)}
  doctor                is this worth driving? build currency, isolation, strays
  run <subcmd> [args]   run one CLI subcommand in the sandbox; capture out/err/exit
  inspect [file]        list the sandbox home, or read one file under it
  cleanup               kill only the pids we started, delete the sandbox, KEEP evidence
  help

Sandbox   ${SBX}
Evidence  ${EVIDENCE}   (survives cleanup)
Ingest    ${DEAD_API}   (nothing listens; events buffer locally)

Isolation: HOME, PROMPTSTER_STATE_DIR, PROMPTSTER_BUFFER_PATH, CODEX_HOME,
CLAUDE_CONFIG_DIR, PROMPTSTER_CURSOR_HOME all point into the sandbox, and
launchctl/systemctl/schtasks are shimmed on PATH because they are keyed on the
UID, not on HOME. See features/README.md.`;

const [cmd, ...rest] = process.argv.slice(2);
let code = 0;
switch (cmd) {
  case "build": code = cmdBuild(); break;
  case "doctor": code = cmdDoctor(); break;
  case "run": code = cmdRun(rest); break;
  case "inspect": code = cmdInspect(rest); break;
  case "cleanup": code = cmdCleanup(); break;
  case "help": case undefined: process.stdout.write(HELP + "\n"); break;
  default: code = fail(`unknown command: ${cmd}`, "Run `help`.");
}
process.exit(code);
