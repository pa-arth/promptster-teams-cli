"use strict";

// Shared path/platform resolution for the npm package.
//
// WHY THIS PACKAGE IS A LAUNCHER, NOT THE INSTALL
// ------------------------------------------------
// The Go binary self-updates by renaming a verified new build over its own
// executable. When the executable it swaps lives inside node_modules, npm's
// metadata immediately starts lying: `npm ls` / `npm outdated` keep reporting
// the version npm installed while the binary underneath has moved on, and a
// reinstall silently writes the older binary back.
//
// You cannot fix that by rewriting package.json behind npm's back — that is a
// hack no mature CLI does, and it does not even work for project-local installs
// (npm ls reads the lockfile, not the installed package.json — verified).
//
// So the binary does not live in node_modules. postinstall.js copies it to the
// MANAGED path (~/.promptster-teams/bin — the same place install.sh writes),
// and bin/promptster-teams.js execs that. node_modules keeps a pristine copy
// that nothing ever mutates, so npm's metadata stays true by construction, and
// self-update stays fully automatic — which matters because this is a daemon:
// unlike claude/codex, there is no human reading a "please upgrade" nudge.
//
// Keep this file dependency-free: postinstall must run before anything is
// installed, and the launcher is on the hot path of every invocation.

const os = require("os");
const path = require("path");

// Maps node's platform-arch to the binary shipped in binaries/.
const PLATFORMS = {
  "darwin-x64": "promptster-teams-darwin-x64",
  "darwin-arm64": "promptster-teams-darwin-arm64",
  "linux-x64": "promptster-teams-linux-x64",
  "linux-arm64": "promptster-teams-linux-arm64",
  "win32-x64": "promptster-teams-win32-x64.exe",
  "win32-arm64": "promptster-teams-win32-arm64.exe",
};

function platformKey() {
  return `${process.platform}-${process.arch}`;
}

// bundledBinPath is the pristine copy npm installed. Never mutated.
function bundledBinPath() {
  const name = PLATFORMS[platformKey()];
  return name ? path.join(__dirname, "..", "binaries", name) : null;
}

// managedBinPath MUST stay in lockstep with Go's state.CanonicalInstallBin and
// install.sh's INSTALL_DIR. All three name the same file; if they diverge, an
// npm install and a curl install manage two different binaries on one box and
// PATH decides which one the engineer actually runs.
function managedBinPath() {
  const home = os.homedir();
  if (!home) return null;
  const name =
    process.platform === "win32" ? "promptster-teams.exe" : "promptster-teams";
  return path.join(home, ".promptster-teams", "bin", name);
}

// parseVersion turns "1.2.3" into [1,2,3]; returns null for anything that is
// not a plain 3-part numeric version. Deliberately strict rather than a semver
// dependency: the release pipeline only ever produces x.y.z, and an unparseable
// version must fall through to "unknown", never to a wrong comparison.
function parseVersion(s) {
  const m = /^v?(\d+)\.(\d+)\.(\d+)/.exec(String(s || "").trim());
  return m ? [Number(m[1]), Number(m[2]), Number(m[3])] : null;
}

// isNewer mirrors the Go updater's strictly-newer gate: a is newer than b.
function isNewer(a, b) {
  const x = parseVersion(a);
  const y = parseVersion(b);
  if (!x || !y) return false;
  for (let i = 0; i < 3; i++) {
    if (x[i] !== y[i]) return x[i] > y[i];
  }
  return false;
}

module.exports = {
  PLATFORMS,
  platformKey,
  bundledBinPath,
  managedBinPath,
  parseVersion,
  isNewer,
};
