#!/usr/bin/env node
"use strict";

// Platform shim: selects and execs the right prebuilt Go binary for this OS/arch.
const { spawnSync } = require("child_process");
const path = require("path");
const fs = require("fs");

const MAP = {
  "darwin-x64": "promptster-teams-darwin-x64",
  "darwin-arm64": "promptster-teams-darwin-arm64",
  "linux-x64": "promptster-teams-linux-x64",
  "linux-arm64": "promptster-teams-linux-arm64",
  "win32-x64": "promptster-teams-win32-x64.exe",
  "win32-arm64": "promptster-teams-win32-arm64.exe",
};

const key = `${process.platform}-${process.arch}`;
const binName = MAP[key];
if (!binName) {
  console.error(`promptster-teams: unsupported platform ${key}`);
  process.exit(1);
}

const binPath = path.join(__dirname, "..", "binaries", binName);
if (!fs.existsSync(binPath)) {
  console.error(`promptster-teams: binary not found at ${binPath}`);
  console.error("The package may have installed incorrectly; try reinstalling.");
  process.exit(1);
}

const res = spawnSync(binPath, process.argv.slice(2), { stdio: "inherit" });
if (res.error) {
  console.error(`promptster-teams: ${res.error.message}`);
  process.exit(1);
}
process.exit(res.status === null ? 1 : res.status);
