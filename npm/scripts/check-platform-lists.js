#!/usr/bin/env node
"use strict";

// Asserts every place that enumerates our platforms agrees, and that the
// wrapper's optionalDependencies pin this exact version.
//
// WHY THIS EXISTS: the platform set is written out in four places that npm and
// Go cannot cross-check for us —
//
//   npm/scripts/build.js       TARGETS      (what gets compiled + packaged)
//   npm/lib/resolve.js         PLATFORMS    (what the launcher will resolve)
//   npm/scripts/check-binaries.js KEYS      (what the publish gate demands)
//   npm/package.json           optionalDependencies (what npm installs)
//
// Adding a platform to build.js but not resolve.js publishes a binary the
// launcher will never look for: engineers on that platform get "no binary
// found" from a release that looks complete. Adding it to package.json but not
// build.js pins an optionalDependency that was never published — and because a
// missing optional dep is a SILENT SUCCESS, `npm i -g` reports success and the
// CLI has no binary. Neither shows up until someone on that exact platform
// installs.
//
// check-binaries.js catches some of this, but only at PREPUBLISH — after a tag
// is cut. This runs on every PR, needs no Go toolchain and no build (it reads
// source, ~50ms), so drift dies at review time instead of release time.
//
// Deliberately parses build.js/resolve.js as TEXT rather than requiring them:
// build.js shells out to `go build` on import, and the point is to read what is
// WRITTEN in each file, not what one of them computes.

const fs = require("fs");
const path = require("path");

const npmDir = path.join(__dirname, "..");
const read = (p) => fs.readFileSync(path.join(npmDir, p), "utf8");

// build.js: TARGETS = [{ ..., key: "darwin-arm64", ... }, ...]
function buildTargets() {
  return [...read("scripts/build.js").matchAll(/key:\s*"([a-z0-9-]+)"/g)].map((m) => m[1]);
}

// resolve.js: const PLATFORMS = [ "darwin-x64", ... ];
function resolvePlatforms() {
  const block = /const PLATFORMS = \[([\s\S]*?)\];/.exec(read("lib/resolve.js"));
  if (!block) return [];
  return [...block[1].matchAll(/"([a-z0-9-]+)"/g)].map((m) => m[1]);
}

// check-binaries.js: const KEYS = ["darwin-x64", ...];
function gateKeys() {
  const block = /const KEYS = \[([\s\S]*?)\];/.exec(read("scripts/check-binaries.js"));
  if (!block) return [];
  return [...block[1].matchAll(/"([a-z0-9-]+)"/g)].map((m) => m[1]);
}

const pkg = JSON.parse(read("package.json"));
const optDeps = Object.keys(pkg.optionalDependencies || {});

const sources = {
  "build.js TARGETS": buildTargets(),
  "resolve.js PLATFORMS": resolvePlatforms(),
  "check-binaries.js KEYS": gateKeys(),
  "package.json optionalDependencies": optDeps.map((n) =>
    n.startsWith(`${pkg.name}-`) ? n.slice(pkg.name.length + 1) : n
  ),
};

const problems = [];

// Every source must be non-empty: a regex that silently stops matching (someone
// reformats the file) would otherwise make this check pass by comparing nothing.
for (const [name, list] of Object.entries(sources)) {
  if (list.length === 0) {
    problems.push(`${name}: parsed ZERO platforms — this check cannot see it any more`);
  }
}

const canonical = sources["build.js TARGETS"];
for (const [name, list] of Object.entries(sources)) {
  if (name === "build.js TARGETS" || list.length === 0) continue;
  const missing = canonical.filter((k) => !list.includes(k));
  const extra = list.filter((k) => !canonical.includes(k));
  for (const k of missing) problems.push(`${name}: missing "${k}" (build.js compiles it)`);
  for (const k of extra) problems.push(`${name}: has "${k}" but build.js does not compile it`);
}

// Pins must be this exact version. A range would let npm resolve a binary from a
// different release than the wrapper: the seven tarballs are ONE artifact.
for (const [name, spec] of Object.entries(pkg.optionalDependencies || {})) {
  if (spec !== pkg.version) {
    problems.push(`${name}: pinned to "${spec}", wrapper is "${pkg.version}"`);
  }
}

if (problems.length > 0) {
  console.error("ERROR: platform definitions are out of sync:\n");
  for (const p of problems) console.error("  - " + p);
  console.error("\nAll four must list the same platforms, and every pin must equal");
  console.error(`the wrapper version (${pkg.version}). Run: node scripts/build.js <version>`);
  process.exit(1);
}

console.log(
  `✓ ${canonical.length} platforms consistent across build.js, resolve.js, ` +
    `check-binaries.js and optionalDependencies (all pinned to ${pkg.version})`
);
