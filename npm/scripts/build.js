#!/usr/bin/env node
"use strict";

/**
 * Cross-compiles the Go CLI for every target platform and emits BOTH:
 *
 *   npm/binaries/   — all six binaries + SHA256SUMS. NOT shipped in the npm
 *                     wrapper any more; these are the GitHub Release assets
 *                     that install.sh and the Go self-updater download.
 *   npm/platforms/  — one publishable package per platform, each carrying ONE
 *                     binary and gated by npm's `os`/`cpu` fields.
 *
 * The wrapper declares the platform packages as optionalDependencies pinned to
 * its own version; npm installs only the one matching the host, so an install
 * pulls ~18MB instead of the ~74.5MB it took when all six shipped in one
 * tarball. Same pattern esbuild/swc/rollup use, and the one Claude Code uses
 * (@anthropic-ai/claude-code-darwin-arm64 et al, each pinned to the wrapper's
 * exact version).
 *
 * Usage: node scripts/build.js [version]
 */

const { execSync } = require("child_process");
const crypto = require("crypto");
const path = require("path");
const fs = require("fs");

const pkgPath = path.resolve(__dirname, "../package.json");
const pkg = JSON.parse(fs.readFileSync(pkgPath, "utf8"));
const version = process.argv[2] || pkg.version || "dev";

// `key` MUST equal `${process.platform}-${process.arch}` — it is what
// resolve.js uses to name the package it require.resolve()s at runtime.
// `os`/`cpu` are what make npm skip the five packages that do not match the
// host; without them every install would download all six again.
const TARGETS = [
  { goos: "linux", goarch: "amd64", out: "promptster-teams-linux-x64", key: "linux-x64", os: "linux", cpu: "x64" },
  { goos: "linux", goarch: "arm64", out: "promptster-teams-linux-arm64", key: "linux-arm64", os: "linux", cpu: "arm64" },
  { goos: "darwin", goarch: "amd64", out: "promptster-teams-darwin-x64", key: "darwin-x64", os: "darwin", cpu: "x64" },
  { goos: "darwin", goarch: "arm64", out: "promptster-teams-darwin-arm64", key: "darwin-arm64", os: "darwin", cpu: "arm64" },
  { goos: "windows", goarch: "amd64", out: "promptster-teams-win32-x64.exe", key: "win32-x64", os: "win32", cpu: "x64" },
  { goos: "windows", goarch: "arm64", out: "promptster-teams-win32-arm64.exe", key: "win32-arm64", os: "win32", cpu: "arm64" },
];

// The binary is named canonically INSIDE each platform package, so resolve.js
// does not need the per-target filenames — only the key.
function platformBinName(key) {
  return key.startsWith("win32-") ? "promptster-teams.exe" : "promptster-teams";
}

const goSrcDir = path.resolve(__dirname, "../..");
const binariesDir = path.resolve(__dirname, "../binaries");
const platformsDir = path.resolve(__dirname, "../platforms");

if (!fs.existsSync(binariesDir)) {
  fs.mkdirSync(binariesDir, { recursive: true });
}
if (!fs.existsSync(path.join(goSrcDir, "go.mod"))) {
  console.error(`Go source (go.mod) not found at: ${goSrcDir}`);
  process.exit(1);
}

console.log(`Building promptster-teams v${version} for all platforms...\n`);

for (const { goos, goarch, out } of TARGETS) {
  const outPath = path.join(binariesDir, out);
  const cmd = `go build -ldflags "-s -w -X github.com/pa-arth/promptster-teams-cli/internal/version.Version=${version}" -o ${outPath} ./cmd/promptster-teams`;
  console.log(`  ${goos}/${goarch} → binaries/${out}`);
  try {
    execSync(cmd, {
      cwd: goSrcDir,
      env: { ...process.env, GOOS: goos, GOARCH: goarch, CGO_ENABLED: "0" },
      stdio: "inherit",
    });
  } catch (err) {
    console.error(`\nFailed to build ${goos}/${goarch}`);
    process.exit(1);
  }
}

// Emit SHA256SUMS covering every built artifact. Uploaded to the GitHub
// Release and used by install.sh to verify a download before executing it.
const sumsLines = TARGETS.map(({ out }) => {
  const buf = fs.readFileSync(path.join(binariesDir, out));
  const hex = crypto.createHash("sha256").update(buf).digest("hex");
  return `${hex}  ${out}`;
});
fs.writeFileSync(path.join(binariesDir, "SHA256SUMS"), sumsLines.join("\n") + "\n");

// --- platform packages ------------------------------------------------------
//
// Rebuilt from scratch each run: a stale platform dir would publish a binary
// from a previous version under the current version's tag.
fs.rmSync(platformsDir, { recursive: true, force: true });
fs.mkdirSync(platformsDir, { recursive: true });

const optionalDependencies = {};
for (const { out, key, os, cpu } of TARGETS) {
  const name = `${pkg.name}-${key}`;
  const dir = path.join(platformsDir, key);
  const binName = platformBinName(key);
  fs.mkdirSync(dir, { recursive: true });
  fs.copyFileSync(path.join(binariesDir, out), path.join(dir, binName));
  fs.chmodSync(path.join(dir, binName), 0o755);
  fs.writeFileSync(
    path.join(dir, "package.json"),
    JSON.stringify(
      {
        name,
        version,
        description: `promptster-teams binary for ${key}`,
        // os/cpu are the whole point: npm silently skips an optional dep whose
        // os/cpu do not match the host, which is what delivers ONE binary.
        os: [os],
        cpu: [cpu],
        license: pkg.license,
        repository: pkg.repository,
        files: [binName],
        publishConfig: { access: "public" },
      },
      null,
      2
    ) + "\n"
  );
  optionalDependencies[name] = version;
}

// Pin optionalDependencies to this exact version, in lockstep. A range would
// let npm resolve a platform binary from a DIFFERENT release than the wrapper —
// the two are one artifact split across seven tarballs, not independent deps.
// Written here rather than hand-maintained so six pins cannot drift from the
// version on a release bump; check-binaries.js re-asserts it before publish.
if (JSON.stringify(pkg.optionalDependencies) !== JSON.stringify(optionalDependencies)) {
  pkg.optionalDependencies = optionalDependencies;
  fs.writeFileSync(pkgPath, JSON.stringify(pkg, null, 2) + "\n");
  console.log(`\n✓ Synced optionalDependencies to ${version}`);
}

console.log("\n✓ All binaries built successfully");
console.log("✓ Wrote binaries/SHA256SUMS");
console.log(`✓ Wrote ${TARGETS.length} platform packages to platforms/`);
console.log(`  Output: ${binariesDir}`);
