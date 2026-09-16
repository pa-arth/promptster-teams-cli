#!/usr/bin/env node
// run-eval — measure how good an agent actually is at verifying this CLI.
//
// The method: inject a real defect into a real .go file, REBUILD, hand a FRESH
// agent session the verify-teams-cli skill and one instruction ("verify X"), and
// see whether it comes back FAIL. Control cases inject nothing and must come
// back PASS. Everything is scored against ground truth we planted, so the
// numbers are not self-reported.
//
// Go is compiled, so the rebuild between injection and run is not optional —
// without it every case would score the agent against the previous binary. The
// runner rebuilds after injecting AND after reverting, and refuses to score a
// case whose rebuild failed.
//
//   node run-eval.mjs --validate            # anchors only, no agents, seconds
//   node run-eval.mjs --agent claude
//   node run-eval.mjs --agent claude --case stop-claims-success-when-nothing-running
//   node run-eval.mjs --all-agents

import fs from "node:fs";
import path from "node:path";
import { spawn, spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const SKILL = path.dirname(HERE);
const REPO = path.resolve(SKILL, "..", "..", "..");
const CTL = path.join(SKILL, "control-teams-cli.mjs");
const CASES = path.join(HERE, "cases");
const RESULTS = path.join(HERE, "results");

const argv = process.argv.slice(2);
const flag = (n, d) => {
  const i = argv.indexOf(`--${n}`);
  if (i === -1) return d;
  const v = argv[i + 1];
  return !v || v.startsWith("--") ? true : v;
};

// A prompt reaches herdr through a file: it is multi-line and would otherwise be
// mangled by argv quoting on the way into an interactive TUI.
const promptFile = (prompt, tag) => {
  const f = path.join(HERE, `.prompt-${tag}-${Date.now()}.txt`);
  fs.writeFileSync(f, prompt);
  return f;
};
const HERDR = path.join(HERE, "herdr-run.mjs");

const AGENTS = {
  // Headless entry points. Fast, but they are a side door: `codex exec` and
  // `cursor-agent -p` are not the binaries a human actually drives, and
  // cursor-agent needs its own CLI login that the desktop app does not provide.
  claude: (prompt) => ["claude", ["-p", prompt, "--permission-mode", "bypassPermissions"]],
  codex: (prompt) => ["codex", ["exec", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", prompt]],
  cursor: (prompt) => ["cursor-agent", ["-p", prompt, "--force"]],

  // Through herdr: drives the SAME interactive binaries the human uses, already
  // signed in, in a throwaway tab that is torn down afterwards. This is what
  // makes a cross-tool comparison honest — every tool is scored as it ships.
  "codex-herdr": (prompt) => ["node", [HERDR, "codex", REPO, promptFile(prompt, "codex")]],
  "cursor-herdr": (prompt) => ["node", [HERDR, "cursor", REPO, promptFile(prompt, "cursor")]],
};

const run = (cmd, args, opts = {}) =>
  new Promise((resolve) => {
    const t0 = Date.now();
    const child = spawn(cmd, args, { cwd: REPO, ...opts });
    let out = "";
    child.stdout.on("data", (d) => (out += d));
    child.stderr.on("data", (d) => (out += d));
    child.on("error", (e) => resolve({ out: String(e.message), ms: Date.now() - t0, code: -1 }));
    child.on("close", (code) => resolve({ out, ms: Date.now() - t0, code }));
  });

const ctl = (...a) => {
  const r = spawnSync("node", [CTL, ...a], { cwd: REPO, encoding: "utf8" });
  try { return JSON.parse(r.stdout); } catch { return { ok: false, error: "control CLI returned non-JSON", raw: (r.stdout || "") + (r.stderr || "") }; }
};

// ------------------------------------------------------------ defect injection

const PENDING = path.join(HERE, ".pending-revert.json");

// A `finally` does not run under SIGKILL, and an OOM kill is exactly what
// happens when agents run concurrently on a loaded machine. Without this, a
// killed run leaves an injected defect in Go source and the next build compiles
// the sabotage. Pristine content goes to disk BEFORE the edit.
function recoverPending() {
  if (!fs.existsSync(PENDING)) return;
  try {
    const { file, original, caseId } = JSON.parse(fs.readFileSync(PENDING, "utf8"));
    fs.writeFileSync(file, original);
    fs.unlinkSync(PENDING);
    console.error(`RECOVERED a previous run of "${caseId}" died with its defect still injected; ${file} restored.`);
  } catch (e) {
    console.error(`WARNING: could not auto-restore from ${PENDING}: ${e.message}. Restore by hand before trusting any build.`);
  }
}

function inject(defect, caseId = "unknown") {
  const file = path.join(REPO, defect.file);
  if (!fs.existsSync(file)) throw new Error(`case targets a file that does not exist: ${defect.file}`);
  const original = fs.readFileSync(file, "utf8");
  const hits = original.split(defect.find).length - 1;
  if (hits !== 1) {
    throw new Error(
      `case anchor must match exactly once in ${defect.file}, matched ${hits}. ` +
      `The CLI changed under the case — fix the case, do not loosen the anchor.`
    );
  }
  fs.writeFileSync(PENDING, JSON.stringify({ file, original, caseId }));
  fs.writeFileSync(file, original.replace(defect.find, defect.replace));
  return () => {
    fs.writeFileSync(file, original);
    try { fs.unlinkSync(PENDING); } catch { /* already cleared */ }
  };
}

// ------------------------------------------------------------ verdict parsing

// The agent is told to end with exactly one VERDICT line. Anything else is an
// invalid run and is scored as such rather than guessed at — an eval that
// guesses the agent's answer measures the parser, not the agent.
function parseVerdict(out) {
  const m = [...out.matchAll(/^\s*VERDICT:\s*(PASS|FAIL)\s*$/gim)].map((x) => x[1].toUpperCase());
  if (!m.length) return { verdict: null, reason: "no VERDICT line" };
  return { verdict: m[m.length - 1], reason: null };
}

const sawEvidence = (out) => /evidence\//.test(out) || /control-teams-cli\.mjs/.test(out);

// ------------------------------------------------------------ validate

function validate() {
  const rows = [];
  for (const f of fs.readdirSync(CASES).filter((x) => x.endsWith(".json")).sort()) {
    const c = JSON.parse(fs.readFileSync(path.join(CASES, f), "utf8"));
    if (!c.defect) { rows.push({ case: c.id, anchor: "control — no defect", ok: true }); continue; }
    const file = path.join(REPO, c.defect.file);
    if (!fs.existsSync(file)) { rows.push({ case: c.id, ok: false, error: `missing file ${c.defect.file}` }); continue; }
    const hits = fs.readFileSync(file, "utf8").split(c.defect.find).length - 1;
    rows.push({ case: c.id, file: c.defect.file, matches: hits, ok: hits === 1, ...(hits === 1 ? {} : { error: "anchor must match EXACTLY ONCE" }) });
  }
  const ok = rows.every((r) => r.ok);
  process.stdout.write(JSON.stringify({ ok, rows, ...(ok ? {} : { hint: "Re-read the named file and re-anchor the case on text that still exists exactly once. Never loosen an anchor to make it match." }) }, null, 2) + "\n");
  process.exit(ok ? 0 : 1);
}

// ------------------------------------------------------------ main

// The verdict is written to a FILE, never scraped from the terminal.
// Scraping was actively wrong: these TUIs echo the prompt, so the instruction
// lines "VERDICT: PASS / or / VERDICT: FAIL" appear in the transcript verbatim.
// The parser took the last match and therefore read MY OWN PROMPT as the
// agent's answer — scoring FAIL for runs where the agent was still sitting at a
// permission dialog and had executed nothing at all.
const PROMPT = (feature, verdictFile) => `Use the verify-teams-cli skill to verify the "${feature}" feature of this Go CLI is working correctly right now.

Build the binary and drive the real commands with the control CLI, in its sandbox. Do not read the Go source to decide your answer — decide from what the built binary actually does. Inspect the resulting sandbox state, not just stdout. Clean up when you are done.

When you have decided, write your verdict to this exact path, as a file whose entire contents are one word, either PASS or FAIL:

${verdictFile}

For example: printf PASS > ${verdictFile}

PASS means the feature works as its feature-map file says it should. FAIL means it does not. Write the file as the last thing you do; a run with no file written is scored as no answer.`;

async function main() {
  recoverPending();
  for (const sig of ["SIGINT", "SIGTERM", "SIGHUP"]) {
    process.on(sig, () => { recoverPending(); process.exit(130); });
  }
  if (flag("validate")) validate();
  fs.mkdirSync(RESULTS, { recursive: true });

  const agentNames = flag("all-agents") ? Object.keys(AGENTS) : [flag("agent", "claude")];
  for (const a of agentNames) if (!AGENTS[a]) { console.error(`unknown agent: ${a}. Known: ${Object.keys(AGENTS).join(", ")}`); process.exit(1); }

  const only = flag("case");
  let cases = fs.readdirSync(CASES).filter((f) => f.endsWith(".json")).sort()
    .map((f) => JSON.parse(fs.readFileSync(path.join(CASES, f), "utf8")));
  if (typeof only === "string") cases = cases.filter((c) => c.id === only);
  if (!cases.length) { console.error("no cases matched"); process.exit(1); }

  // Fail fast rather than scoring an agent against a toolchain that cannot build.
  ctl("cleanup");
  const health = ctl("doctor");
  if (!health?.checks?.goAvailable) {
    console.error(JSON.stringify({ ok: false, error: "no Go toolchain", hint: "Install Go. Scoring an agent against a CLI that cannot be built measures nothing.", doctor: health }, null, 2));
    process.exit(1);
  }

  // Verdicts are NOT deterministic: the same agent on the same healthy build
  // returned FAIL once and PASS on rerun. A single run per case measures noise
  // as much as skill, so repeats are first-class rather than something you
  // remember to do by hand.
  const REPEAT = Math.max(1, Number(flag("repeat", 1)) || 1);
  const runs = [];
  for (const agent of agentNames) {
    for (const c0 of cases) {
     for (let rep = 1; rep <= REPEAT; rep++) {
      const c = c0;
      let revert = () => {};
      try {
        if (c.defect) revert = inject(c.defect);
        // Go is compiled: without this the agent drives the PREVIOUS binary.
        const b = ctl("build");
        if (!b.ok) throw new Error(`rebuild after injection failed: ${JSON.stringify(b.compileError ?? b.error)}`);

        const verdictFile = path.join(HERE, `.verdict-${agent}-${c.id}-${rep}.txt`);
        try { fs.unlinkSync(verdictFile); } catch { /* none from a previous run */ }
        const [cmd, args] = AGENTS[agent](PROMPT(c.feature, verdictFile));
        const res = await run(cmd, args);
        let verdict = null, reason = null;
        if (fs.existsSync(verdictFile)) {
          const raw = fs.readFileSync(verdictFile, "utf8").trim().toUpperCase();
          if (raw === "PASS" || raw === "FAIL") verdict = raw;
          else reason = `verdict file held ${JSON.stringify(raw.slice(0, 40))}, not PASS or FAIL`;
          try { fs.unlinkSync(verdictFile); } catch { /* best effort */ }
        } else {
          reason = "agent wrote no verdict file";
        }
        const correct = verdict === null ? null : verdict === c.expect;
        runs.push({
          agent, case: c.id, rep, feature: c.feature, expect: c.expect, got: verdict, correct,
          invalid: reason, evidence: sawEvidence(res.out), ms: res.ms, detects: c.detects ?? null,
          // An invalid run that keeps no transcript cannot be diagnosed, and an
          // eval you cannot diagnose is an eval you cannot trust. Keep the tail
          // whenever the agent did not produce a clean verdict.
          // Keep the transcript whenever the run was not cleanly correct. A wrong
          // verdict is the most interesting outcome there is — it is the one that
          // tells you whether the skill misled the agent or the agent misread it.
          transcriptTail: (reason || correct === false) ? String(res.out).slice(-6000) : undefined,
          exitCode: (reason || correct === false) ? res.code : undefined,
        });
        console.error(`${correct === true ? "HIT " : correct === false ? "MISS" : "INV "} ${agent}/${c.id}${REPEAT > 1 ? ` [${rep}/${REPEAT}]` : ""} expect=${c.expect} got=${verdict ?? "-"} ${(res.ms / 1000).toFixed(0)}s`);
      } catch (e) {
        runs.push({ agent, case: c.id, error: String(e.message || e) });
        console.error(`ERR  ${agent}/${c.id}: ${e.message || e}`);
      } finally {
        revert();
        // Leave the tree AND the binary clean, so the next case (or a human
        // running `make build`) is not silently holding a defective build.
        ctl("build");
        // Memory: agents leave detached sandbox daemons behind.
        ctl("cleanup");
        // Let the previous agent's tab finish tearing down. Creating the next
        // one on top of a closing tab is what produced sub-second invalid runs.
        await new Promise((r) => setTimeout(r, 4000));
      }
     }
    }
  }

  const score = (agent) => {
    const mine = runs.filter((r) => r.agent === agent && !r.error);
    const broken = mine.filter((r) => r.expect === "FAIL");
    const healthy = mine.filter((r) => r.expect === "PASS");
    const pct = (n, d) => (d ? Math.round((n / d) * 100) : null);
    return {
      agent, scored: mine.length,
      detection: { caught: broken.filter((r) => r.correct).length, of: broken.length, pct: pct(broken.filter((r) => r.correct).length, broken.length) },
      falseAlarm: { wrongFails: healthy.filter((r) => r.correct === false).length, of: healthy.length, pct: pct(healthy.filter((r) => r.correct === false).length, healthy.length) },
      invalidVerdicts: mine.filter((r) => r.invalid).length,
      evidenceCaptured: { n: mine.filter((r) => r.evidence).length, of: mine.length, pct: pct(mine.filter((r) => r.evidence).length, mine.length) },
      medianSeconds: mine.length ? Math.round(mine.map((r) => r.ms).sort((a, b) => a - b)[Math.floor(mine.length / 2)] / 1000) : null,
      // Flaky = the same case, same build, answered differently across repeats.
      // This is a property of the agent, not of the defect, and it caps how much
      // any single run is worth believing.
      flaky: (() => {
        const byCase = {};
        for (const r of mine) (byCase[r.case] ??= new Set()).add(r.got ?? "none");
        const repeated = Object.entries(byCase).filter(([, v]) => v.size > 0);
        const mixed = repeated.filter(([, v]) => v.size > 1).map(([k]) => k);
        return { cases: mixed, n: mixed.length, of: repeated.length, note: mixed.length ? "these cases did not answer consistently; a single run of them is noise" : undefined };
      })(),
    };
  };

  const report = { ranAt: new Date().toISOString(), scores: agentNames.map(score), runs };
  const file = path.join(RESULTS, `${Date.now()}.json`);
  fs.writeFileSync(file, JSON.stringify(report, null, 2));
  process.stdout.write(JSON.stringify({ ...report, runs: undefined, reportFile: file }, null, 2) + "\n");
}

main().catch((e) => { console.error(e); process.exit(1); });
