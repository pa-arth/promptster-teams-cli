#!/usr/bin/env node
"use strict";

// Installs the platform binary from node_modules to the MANAGED path
// (~/.promptster-teams/bin/promptster-teams), which is what actually runs and
// what self-update owns. See lib/resolve.js for why the binary must not run
// from inside node_modules.
//
// Contract: this script must NEVER fail an npm install. A postinstall that
// exits non-zero aborts `npm i -g` and leaves the engineer with no CLI at all —
// far worse than the drift it exists to fix. Every failure path warns and exits
// 0; bin/promptster-teams.js then falls back to the bundled binary, which works
// exactly as it did before this file existed.

const { spawnSync } = require("child_process");
const fs = require("fs");
const path = require("path");

const {
  bundledBinPath,
  managedBinPath,
  isGlobalInstall,
  isNewer,
  platformKey,
} = require("../lib/resolve");

function warn(msg) {
  console.warn(`promptster-teams: ${msg}`);
}

// binVersion asks a binary what it is. One that cannot answer (missing,
// corrupt, wrong arch, not executable) reports null.
//
// Both sides of the downgrade guard go through this deliberately. The obvious
// shortcut is to compare package.json's version against the managed binary —
// but that trusts package.json to describe the bytes sitting in binaries/, and
// when those two disagree the guard makes its decision on a fiction (and the
// success log prints a version that was never installed). The pipeline does gate
// package.json against the tag, so they agree today; comparing the actual binary
// to the actual binary means the guard cannot be wrong even if that gate is ever
// lost. It costs one ~10ms spawn on install.
function binVersion(bin) {
  if (!bin || !fs.existsSync(bin)) return null;
  try {
    const r = spawnSync(bin, ["--version"], {
      encoding: "utf8",
      timeout: 10_000,
    });
    if (r.status !== 0 || !r.stdout) return null;
    return r.stdout.trim().split("\n")[0].trim() || null;
  } catch {
    return null;
  }
}

function main() {
  // A project-local install must not touch the shared managed binary. The
  // lockfile is a per-PROJECT pin; the managed binary is per-USER. Letting a
  // local install write it means two repos pinning different versions fight
  // over one file and neither gets what its lockfile selected — a worse bug
  // than the npm-ls drift this whole design removes. Local installs keep
  // running out of their own node_modules, exactly as before.
  if (!isGlobalInstall()) {
    console.log(
      "promptster-teams: project-local install — leaving ~/.promptster-teams/bin alone " +
        "and running the version your lockfile pins"
    );
    return;
  }

  const bundled = bundledBinPath();
  if (!bundled) {
    warn(`unsupported platform ${platformKey()} — skipping binary install`);
    return;
  }
  if (!fs.existsSync(bundled)) {
    warn(`bundled binary missing at ${bundled} — skipping`);
    return;
  }
  const managed = managedBinPath();
  if (!managed) {
    warn("could not resolve home directory — skipping binary install");
    return;
  }

  const incoming = binVersion(bundled);
  const current = binVersion(managed);

  if (!incoming) {
    warn(`bundled binary at ${bundled} did not report a version — skipping`);
    return;
  }

  // Never downgrade. The managed binary self-updates forward on its own, so it
  // is routinely NEWER than whatever version npm is installing (that is the
  // normal steady state, not an error). Clobbering it would hand the daemon an
  // older build that immediately re-updates — churn, plus a window on a version
  // the engineer already moved past. Mirrors the Go updater's isNewer gate.
  //
  // A managed binary that cannot report a version (current === null) is treated
  // as absent and overwritten: a corrupt or half-written file should be
  // replaced, not preserved by a guard meant to protect a GOOD newer build.
  if (current && !isNewer(incoming, current)) {
    console.log(
      `promptster-teams: ${managed} is ${current}; not replacing it with ${incoming}`
    );
    return;
  }

  try {
    fs.mkdirSync(path.dirname(managed), { recursive: true });
    // Write to a temp file in the SAME directory, then rename: rename is atomic
    // on POSIX, so a concurrent `promptster-teams` exec sees either the whole
    // old binary or the whole new one, never a half-written file. Copying
    // straight onto `managed` would also fail with ETXTBSY on Linux if the
    // daemon is running.
    const tmp = `${managed}.tmp-${process.pid}`;
    fs.copyFileSync(bundled, tmp);
    fs.chmodSync(tmp, 0o755);

    // Re-check immediately before the swap. The guard above and this rename are
    // not atomic with respect to the Go self-updater, which renames onto this
    // same path from a live daemon: read 1.0.0 -> daemon installs 1.2.0 ->
    // rename 1.1.0 over it, and the guard has been defeated.
    //
    // This narrows the window from "the whole copy" to the microseconds around
    // the rename rather than closing it. Closing it needs a lock protocol shared
    // by a Node script and a Go daemon, which is a lot of machinery for this
    // failure: rename is atomic so the file is always ONE whole valid binary
    // (never corrupt), the only cost is running an older version, and the daemon
    // re-updates forward within one check interval (<=30m). Deliberate tradeoff,
    // not an oversight — see CLAUDE.md.
    const stillCurrent = binVersion(managed);
    if (stillCurrent && !isNewer(incoming, stillCurrent)) {
      fs.unlinkSync(tmp);
      console.log(
        `promptster-teams: ${managed} changed to ${stillCurrent} mid-install; leaving it`
      );
      return;
    }
    fs.renameSync(tmp, managed);
    console.log(
      `promptster-teams: installed ${incoming} to ${managed}${
        current ? ` (was ${current})` : ""
      }`
    );
  } catch (err) {
    // Falls back to the bundled binary via bin/promptster-teams.js.
    warn(`could not install to ${managed}: ${err.message}`);
    warn("falling back to the bundled binary (npm ls may report a stale version)");
  }
}

try {
  main();
} catch (err) {
  // Belt and braces: nothing here may abort an npm install.
  warn(`postinstall skipped: ${err && err.message}`);
}
