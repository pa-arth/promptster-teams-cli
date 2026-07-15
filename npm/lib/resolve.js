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
// That pristine copy arrives via a per-platform optionalDependency gated on
// npm's os/cpu fields, so an install downloads ONE binary (~18MB) instead of the
// six (~74.5MB) that used to ship in a single tarball. The tradeoff: optional
// deps fail SILENTLY, so every consumer of bundledBinPath() must handle null.
//
// Keep this file dependency-free: postinstall must run before anything is
// installed, and the launcher is on the hot path of every invocation.

const fs = require("fs");
const os = require("os");
const path = require("path");

// The platform-arch keys we publish a binary package for. The package name is
// always `<wrapper>-<key>` and the binary inside is always named canonically,
// so the key is the only thing that varies.
const PLATFORMS = [
  "darwin-x64",
  "darwin-arm64",
  "linux-x64",
  "linux-arm64",
  "win32-x64",
  "win32-arm64",
];

const PKG_PREFIX = "@promptster/teams-cli";

function platformKey() {
  return `${process.platform}-${process.arch}`;
}

function platformPackage(key) {
  return `${PKG_PREFIX}-${key || platformKey()}`;
}

// bundledBinPath is the pristine copy npm installed — now inside the per-platform
// optionalDependency (@promptster/teams-cli-darwin-arm64, …) rather than a
// binaries/ directory carrying all six. npm skips the packages whose os/cpu do
// not match the host, so exactly one is ever present.
//
// Returns null when the platform package is absent. That is a REAL state, not a
// theoretical one: optional dependencies fail SILENTLY by design, so
// --omit=optional, a partial registry publish, or an unsupported platform all
// land here with npm reporting success. Callers must degrade, never assume.
//
// Resolved via require.resolve rather than a hardcoded ../../ path so it works
// wherever the package manager actually put it — hoisted to a root
// node_modules, nested, or in a pnpm store.
function bundledBinPath() {
  const key = platformKey();
  if (!PLATFORMS.includes(key)) return null;
  const binName = key.startsWith("win32-")
    ? "promptster-teams.exe"
    : "promptster-teams";
  try {
    const pkgJson = require.resolve(`${platformPackage(key)}/package.json`);
    const p = path.join(path.dirname(pkgJson), binName);
    return fs.existsSync(p) ? p : null;
  } catch {
    return null;
  }
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

// isGlobalInstall reports whether this package was installed into a GLOBAL
// prefix rather than a project's node_modules.
//
// This gate is why a project-local install does not touch the managed binary at
// all. The managed binary is per-USER and shared; a project's lockfile is a
// deliberate per-PROJECT pin. If a local install pointed at the shared binary,
// a repo pinning 0.5.0 and one pinning 0.6.1 would both execute whichever
// version happens to be in ~/.promptster-teams/bin — the lockfile would select
// nothing at all. That is strictly worse than the npm-ls drift this design
// removes, so local installs stay entirely inside their own node_modules.
//
// Mirrors nudgeFor's layout rules in internal/selfupdate/selfupdate.go: match
// path SEGMENTS (never substrings — a directory called my-node_modules-backup
// is not npm), and split on both separators so the check is host-independent.
function pathSegments(p) {
  return String(p || "").split(/[/\\]/).filter(Boolean);
}

function isGlobalInstall(selfDir) {
  const segs = pathSegments(selfDir === undefined ? __dirname : selfDir);
  if (!segs.includes("node_modules")) {
    // Not under node_modules at all (e.g. `npm link`, a checkout). Not a
    // project-local install, so it may manage the shared binary.
    return true;
  }
  const pnpm = segs.includes(".pnpm") || segs.includes("pnpm");
  const adjacent = (a, b) =>
    segs.some((s, i) => s === a && segs[i + 1] === b);

  // pnpm's global prefix, e.g. ~/Library/pnpm/global/5/node_modules/...
  if (pnpm && segs.includes("global")) return true;
  // npm's global prefix: <prefix>/lib/node_modules (unix) or
  // <AppData>\npm\node_modules (windows).
  if (!pnpm && (adjacent("lib", "node_modules") || adjacent("npm", "node_modules"))) {
    return true;
  }
  return false;
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
  platformPackage,
  bundledBinPath,
  managedBinPath,
  isGlobalInstall,
  parseVersion,
  isNewer,
};
