#!/usr/bin/env node
"use strict";

/**
 * Cross-compiles the Go CLI for all target platforms into npm/binaries/.
 * Usage: node scripts/build.js [version]
 */

const { execSync } = require("child_process");
const crypto = require("crypto");
const path = require("path");
const fs = require("fs");

const pkg = JSON.parse(fs.readFileSync(path.resolve(__dirname, "../package.json"), "utf8"));
const version = process.argv[2] || pkg.version || "dev";

const TARGETS = [
  { goos: "linux", goarch: "amd64", out: "promptster-teams-linux-x64" },
  { goos: "linux", goarch: "arm64", out: "promptster-teams-linux-arm64" },
  { goos: "darwin", goarch: "amd64", out: "promptster-teams-darwin-x64" },
  { goos: "darwin", goarch: "arm64", out: "promptster-teams-darwin-arm64" },
  { goos: "windows", goarch: "amd64", out: "promptster-teams-win32-x64.exe" },
  { goos: "windows", goarch: "arm64", out: "promptster-teams-win32-arm64.exe" },
];

const goSrcDir = path.resolve(__dirname, "../..");
const binariesDir = path.resolve(__dirname, "../binaries");

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

console.log("\n✓ All binaries built successfully");
console.log("✓ Wrote binaries/SHA256SUMS");
console.log(`  Output: ${binariesDir}`);
