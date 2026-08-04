#!/usr/bin/env node
"use strict";

// Launcher: execs the MANAGED binary (~/.promptster-teams/bin), falling back to
// the copy bundled in node_modules. See lib/resolve.js for why the binary that
// runs must not be the one npm is tracking.
//
// Preferring `managed` is what keeps npm honest: self-update rewrites the
// managed file, node_modules is never touched, so `npm ls` keeps telling the
// truth about what npm installed.

const { spawnSync } = require("child_process");
const fs = require("fs");

const {
  PLATFORMS,
  bundledBinPath,
  managedBinPath,
  isGlobalInstall,
  platformKey,
  platformPackage,
} = require("../lib/resolve");

function usable(p) {
  if (!p) return false;
  try {
    fs.accessSync(p, fs.constants.X_OK);
    return true;
  } catch {
    return false;
  }
}

// Converge the managed binary before choosing which one to run.
//
// npm is not a reliable place to hang this work any more: its install-script
// approval gate can decline to run our postinstall while reporting a completely
// successful install, so the managed binary — the one the daemon executes and
// autostart points at — is never written and `autostart repair` never runs. The
// engineer sees "added 2 packages" and a working CLI, and only the parts that
// matter are missing. Doing it here means running the CLI at all converges the
// machine, whatever npm decided about our scripts.
//
// Cost when there is nothing to do (the overwhelming case): one readFileSync of
// a tiny marker file. No spawns, no stat of the bundled binary.
//
// `uninstall` is excluded — reinstalling the binary a person is in the middle of
// removing is the one thing this must never do.
try {
  const {
    installManagedBinary,
    mayNeedInstall,
    shouldConvergeOnInvocation,
  } = require("../lib/install");
  if (shouldConvergeOnInvocation(process.argv[2])) {
    if (mayNeedInstall(require("../package.json").version)) {
      // stderr, never stdout: this must not corrupt the output of the command
      // the engineer actually ran (`version`, `status --json`, …).
      const say = (msg) => console.error(`promptster-teams: ${msg}`);
      installManagedBinary({ log: say, warn: say });
    }
  }
} catch (err) {
  // Never block the command. The worst case is the pre-existing behaviour: run
  // the bundled binary and let npm's metadata drift.
  console.error(`promptster-teams: could not check the managed install: ${err && err.message}`);
}

const bundled = bundledBinPath();

// A project-local install runs ITS OWN binary, never the shared managed one.
// The managed binary is per-user; a lockfile is a per-project pin. Pointing a
// local install at the shared file would mean a repo pinning 0.5.0 executes
// whatever another repo last installed — the lockfile would select nothing.
//
// Otherwise: managed first. If postinstall could not write it (--ignore-scripts,
// read-only home, unresolvable HOME), fall back to bundled so the CLI still runs
// — it just self-updates inside node_modules the way it used to, and npm ls goes
// back to drifting. Working-but-drifting beats not working.
const managed = isGlobalInstall() ? managedBinPath() : null;
const binPath = usable(managed) ? managed : bundled;

if (!usable(binPath)) {
  // The binary now arrives as a per-platform optionalDependency, and npm treats
  // a missing optional dep as a SUCCESSFUL install — no error, no warning. So
  // this is the first and only place an engineer learns anything is wrong, and
  // a bare "binary not found" would send them hunting the wrong thing. Name the
  // package that is actually missing and the most likely cause.
  if (!PLATFORMS.includes(platformKey())) {
    console.error(`promptster-teams: unsupported platform ${platformKey()}`);
    console.error(`Supported: ${PLATFORMS.join(", ")}`);
    process.exit(1);
  }
  console.error(
    `promptster-teams: no binary found — ${platformPackage()} is not installed.`
  );
  console.error("This usually means the install used --omit=optional/--no-optional.");
  console.error("Fix: npm i -g @promptster/teams-cli   (without omitting optional deps)");
  process.exit(1);
}

const res = spawnSync(binPath, process.argv.slice(2), { stdio: "inherit" });
if (res.error) {
  console.error(`promptster-teams: ${res.error.message}`);
  process.exit(1);
}
process.exit(res.status === null ? 1 : res.status);
