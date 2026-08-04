#!/usr/bin/env node
"use strict";

// Installs the platform binary from node_modules to the MANAGED path
// (~/.promptster-teams/bin/promptster-teams), which is what actually runs and
// what self-update owns. See lib/resolve.js for why the binary must not run
// from inside node_modules, and lib/install.js for the install itself.
//
// This is no longer the ONLY path to that install: npm can decline to run
// install scripts at all (its allow-scripts approval gate, and --ignore-scripts),
// and it says so in a warning that reads like housekeeping while the install
// silently does nothing. So bin/promptster-teams.js performs the same converge
// on invocation. This file stays because running it at install time is still
// strictly better — the binary is in place before the engineer's first command,
// and `autostart repair` lands during the upgrade rather than after it.
//
// Contract: this script must NEVER fail an npm install. A postinstall that exits
// non-zero aborts `npm i -g` and leaves the engineer with no CLI at all — far
// worse than the drift it exists to fix. Every failure path warns and exits 0;
// the launcher then falls back to the bundled binary, which works exactly as it
// did before this file existed.

const { installManagedBinary } = require("../lib/install");
const { isGlobalInstall } = require("../lib/resolve");

const log = (msg) => console.log(`promptster-teams: ${msg}`);
const warn = (msg) => console.warn(`promptster-teams: ${msg}`);

try {
  if (!isGlobalInstall()) {
    log(
      "project-local install — leaving ~/.promptster-teams/bin alone " +
        "and running the version your lockfile pins"
    );
  } else {
    installManagedBinary({ log, warn });
  }
} catch (err) {
  // Belt and braces: nothing here may abort an npm install.
  warn(`postinstall skipped: ${err && err.message}`);
}
