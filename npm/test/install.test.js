"use strict";

// Tests for the managed-binary install, which is now reachable from BOTH the
// postinstall and the launcher — because npm can decline to run install scripts
// while reporting a successful install.
//
// Fixtures are shell scripts that print a version, not real binaries: the
// install only ever asks a file `--version` and copies it, so a script exercises
// every branch. Windows is skipped rather than faked — the npm CI job runs on
// ubuntu, and a fake that cannot run is worse than an honest skip.

const test = require("node:test");
const assert = require("node:assert");
const fs = require("fs");
const os = require("os");
const path = require("path");

const {
  installManagedBinary,
  mayNeedInstall,
  shouldConvergeOnInvocation,
} = require("../lib/install");
const { managedBinPath } = require("../lib/resolve");

const WINDOWS = process.platform === "win32";

// sandbox points os.homedir() (and therefore the managed path) at a throwaway
// dir, so no test can touch the developer's real ~/.promptster-teams/bin.
function sandbox(t) {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), "pt-npm-"));
  const prev = process.env.HOME;
  process.env.HOME = home;
  t.after(() => {
    process.env.HOME = prev;
    fs.rmSync(home, { recursive: true, force: true });
  });
  return home;
}

function fakeBinary(dir, version) {
  fs.mkdirSync(dir, { recursive: true });
  const p = path.join(dir, `pt-${version}`);
  fs.writeFileSync(p, `#!/bin/sh\necho ${version}\n`, { mode: 0o755 });
  return p;
}

const silent = { log() {}, warn() {} };

test("installs the bundled binary when nothing is managed yet", { skip: WINDOWS }, (t) => {
  const home = sandbox(t);
  const bundled = fakeBinary(path.join(home, "node_modules"), "0.12.2");

  const result = installManagedBinary({ ...silent, bundled });

  assert.strictEqual(result, "installed");
  const managed = managedBinPath();
  assert.ok(fs.existsSync(managed), "managed binary should exist");
  assert.match(fs.readFileSync(managed, "utf8"), /0\.12\.2/);
});

// The managed binary self-updates FORWARD on its own, so it is routinely newer
// than the version npm is installing. That is the steady state, not an error —
// clobbering it hands the daemon an older build that immediately re-updates.
test("never downgrades a managed binary that is already newer", { skip: WINDOWS }, (t) => {
  const home = sandbox(t);
  const managed = managedBinPath();
  fs.mkdirSync(path.dirname(managed), { recursive: true });
  fs.writeFileSync(managed, "#!/bin/sh\necho 0.13.0\n", { mode: 0o755 });

  const bundled = fakeBinary(path.join(home, "node_modules"), "0.12.2");
  const result = installManagedBinary({ ...silent, bundled });

  assert.strictEqual(result, "already-current");
  assert.match(fs.readFileSync(managed, "utf8"), /0\.13\.0/, "must keep the newer build");
});

// The launcher consults mayNeedInstall on EVERY invocation, so a false answer
// has to be conclusive and cheap. A missing marker (the exact state left by an
// install whose postinstall never ran) must read as "look properly".
test("mayNeedInstall gates the launcher's cheap path", { skip: WINDOWS }, (t) => {
  const home = sandbox(t);
  assert.strictEqual(mayNeedInstall("0.12.2"), true, "no managed binary at all");

  const bundled = fakeBinary(path.join(home, "node_modules"), "0.12.2");
  installManagedBinary({ ...silent, bundled });

  assert.strictEqual(mayNeedInstall("0.12.2"), false, "same version: no work, no spawns");
  assert.strictEqual(mayNeedInstall("0.13.0"), true, "a newer wrapper must be considered");

  fs.rmSync(path.join(path.dirname(managedBinPath()), ".npm-installed"));
  assert.strictEqual(mayNeedInstall("0.12.2"), true, "a missing marker is not proof of anything");
});

// Skipping the write still has to record what was evaluated, or the launcher's
// gate stays open and re-probes two binaries on every single command.
test("records the evaluated version even when it writes nothing", { skip: WINDOWS }, (t) => {
  const home = sandbox(t);
  const managed = managedBinPath();
  fs.mkdirSync(path.dirname(managed), { recursive: true });
  fs.writeFileSync(managed, "#!/bin/sh\necho 0.13.0\n", { mode: 0o755 });

  const bundled = fakeBinary(path.join(home, "node_modules"), "0.12.2");
  installManagedBinary({ ...silent, bundled });

  assert.strictEqual(mayNeedInstall("0.12.2"), false, "second run must not re-probe");
});

// `uninstall` is the only working uninstall path there is (npm runs no uninstall
// lifecycle script), so a launcher that reinstalled the binary on the way into
// it would leave the engineer no lever at all.
test("the launcher does not converge on its way into `uninstall`", () => {
  assert.strictEqual(shouldConvergeOnInvocation("uninstall"), false);
  assert.strictEqual(shouldConvergeOnInvocation("doctor"), true);
  assert.strictEqual(shouldConvergeOnInvocation(undefined), true);
});
