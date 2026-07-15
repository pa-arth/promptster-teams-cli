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
