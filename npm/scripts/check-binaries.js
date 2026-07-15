#!/usr/bin/env node
"use strict";

const { execFileSync } = require("child_process");
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

// The tarball MUST carry the postinstall and the module it requires. Without
// them the package silently reverts to running the binary out of node_modules,
// which is the exact drift this design removes — and it reverts SILENTLY: the
// CLI still works, so nothing fails until someone notices `npm ls` lying weeks
// later.
//
// This is a live footgun, not a hypothetical: npm/.npmignore lists `scripts/`.
// package.json's `files` allowlist currently wins, so postinstall.js does ship
// (verified with `npm pack --dry-run`) — but that outcome hinges on a precedence
// rule between two files that both look authoritative, and trimming `files`
// would break it with no other signal. Ask the packer what is actually in the
// tarball rather than trusting either file.
const REQUIRED_IN_TARBALL = ["scripts/postinstall.js", "lib/resolve.js"];

let entries;
try {
  const out = execFileSync("npm", ["pack", "--dry-run", "--json"], {
    cwd: path.join(__dirname, ".."),
    encoding: "utf8",
    stdio: ["ignore", "pipe", "ignore"],
  });
  entries = JSON.parse(out)[0].files.map((f) => f.path);
} catch (err) {
  console.error(`ERROR: could not inspect tarball contents: ${err.message}`);
  process.exit(1);
}

const absent = REQUIRED_IN_TARBALL.filter((f) => !entries.includes(f));
if (absent.length > 0) {
  console.error("ERROR: files missing from the tarball:");
  for (const f of absent) console.error("  - " + f);
  console.error(
    "\nWithout these, npm installs run the binary from node_modules and `npm ls`"
  );
  console.error("goes stale on the first self-update. Check `files` in");
  console.error("package.json and npm/.npmignore.");
  process.exit(1);
}

console.log("✓ postinstall + resolve ship in the tarball");
