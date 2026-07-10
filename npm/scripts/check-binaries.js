#!/usr/bin/env node
"use strict";

const fs = require("fs");
const path = require("path");

const EXPECTED = [
  "promptster-teams-linux-x64",
  "promptster-teams-linux-arm64",
  "promptster-teams-darwin-x64",
  "promptster-teams-darwin-arm64",
  "promptster-teams-win32-x64.exe",
  "promptster-teams-win32-arm64.exe",
];

const binDir = path.join(__dirname, "..", "binaries");
// SHA256SUMS is a required release artifact — install.sh verifies against it.
const missing = [...EXPECTED, "SHA256SUMS"].filter(
  (name) => !fs.existsSync(path.join(binDir, name)),
);

if (missing.length > 0) {
  console.error("ERROR: Missing binaries before publish:");
  for (const m of missing) console.error("  - " + m);
  console.error("\nRun: node scripts/build.js");
  process.exit(1);
}

console.log("✓ All binaries present");
