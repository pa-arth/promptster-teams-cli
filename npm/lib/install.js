"use strict";

// Installing the managed binary, shared by postinstall AND the launcher.
//
// WHY THE LAUNCHER ALSO DOES THIS
// -------------------------------
// postinstall used to be the only path, and postinstall is not guaranteed to
// run. npm gained an install-script approval gate, and a machine that has not
// approved ours reports a completely successful `npm i -g` while doing none of
// the work this package exists to do:
//
//   npm warn allow-scripts 1 package has install scripts not yet covered by allowScripts
//
// Nothing about that is loud. The engineer sees "added 2 packages", the CLI
// still runs (the launcher falls back to the bundled copy), and the two things
// that silently do not happen are the two that matter: the managed binary — the
// one the daemon runs, autostart points at, and self-update owns — is never
// written, and `autostart repair` never re-points a unit at it. Observed in the
// field on npm's newer gate, where `npm approve-scripts --allow-scripts-pending`
// answered "No packages with unreviewed install scripts" and the next install
// warned again.
//
// So the install is idempotent and reachable from the launcher too. Running the
// CLI at all is now enough to converge the machine, whatever the package manager
// decided about our scripts. --ignore-scripts lands in the same place.
//
// Everything here is best-effort by contract. It runs inside `npm install`,
// where a throw aborts the install and leaves the engineer with no CLI at all,
// and on the hot path of every CLI invocation, where a throw breaks a command
// that had nothing to do with installing. Every path returns a result; none
// throw.

const { spawnSync } = require("child_process");
const fs = require("fs");
const path = require("path");

const {
  PLATFORMS,
  bundledBinPath,
  managedBinPath,
  isGlobalInstall,
  isNewer,
  platformKey,
  platformPackage,
} = require("./resolve");

// markerPath records the BUNDLED version last evaluated against the managed
// binary. It exists purely to keep the launcher's check cheap: without it every
// invocation would spawn two `--version` probes (~20ms) to answer "nothing to
// do", which is the answer approximately always.
//
// It is a HINT, never the decision. The real guard still compares binary to
// binary — see installManagedBinary.
function markerPath() {
  const managed = managedBinPath();
  return managed ? path.join(path.dirname(managed), ".npm-installed") : null;
}

function readMarker() {
  try {
    const p = markerPath();
    return p ? fs.readFileSync(p, "utf8").trim() : null;
  } catch {
    return null;
  }
}

function writeMarker(version) {
  try {
    const p = markerPath();
    if (!p) return;
    fs.mkdirSync(path.dirname(p), { recursive: true });
    fs.writeFileSync(p, `${version}\n`, { mode: 0o600 });
  } catch {
    // A missing marker costs two spawns on the next run, nothing more.
  }
}

// mayNeedInstall is the cheap gate the launcher consults on every invocation:
// one readFileSync of a ~7-byte file, no spawns.
//
// It answers true when the managed binary is absent, or when the marker is older
// than the version npm has on disk (or missing entirely — an install whose
// postinstall never ran leaves no marker). A false answer must be conclusive;
// a true answer only buys the right to look properly.
function mayNeedInstall(bundledPkgVersion) {
  const managed = managedBinPath();
  if (!managed) return false;
  if (!fs.existsSync(managed)) return true;
  const marker = readMarker();
  if (!marker) return true;
  return isNewer(bundledPkgVersion, marker);
}

// binVersion asks a binary what it is. One that cannot answer (missing, corrupt,
// wrong arch, not executable) reports null.
//
// Both sides of the downgrade guard go through this deliberately. The obvious
// shortcut is to compare package.json's version against the managed binary — but
// that trusts package.json to describe the bytes actually shipped, and when those
// two disagree the guard decides on a fiction (and the success log prints a
// version that was never installed). Comparing the actual binary to the actual
// binary means the guard cannot be wrong even if the pipeline's package.json gate
// is ever lost. It costs one ~10ms spawn.
function binVersion(bin) {
  if (!bin || !fs.existsSync(bin)) return null;
  try {
    const r = spawnSync(bin, ["--version"], { encoding: "utf8", timeout: 10_000 });
    if (r.status !== 0 || !r.stdout) return null;
    return r.stdout.trim().split("\n")[0].trim() || null;
  } catch {
    return null;
  }
}

// repairAutostart re-points an ALREADY-ENABLED launchd/systemd/schtasks unit at
// the managed binary.
//
// THIS IS A MIGRATION, and without it this package is a silent regression for
// every engineer who has run `autostart enable`. The unit bakes an ABSOLUTE path
// at enable time. Installs predating the managed-binary layout baked
// <pkg>/binaries/promptster-teams-<platform> — a path that no longer exists in
// the package, because the binary moved to a per-platform dependency and the
// wrapper stopped shipping binaries/ entirely. So `npm i -g` deletes the exact
// file the supervisor is pointed at.
//
// Nothing about that fails loudly. The running daemon holds its inode and keeps
// capturing, so the upgrade looks clean; the unit only breaks at the NEXT LOGIN,
// when launchd runs a path that is gone. Capture never comes back — the precise
// failure autostart exists to prevent.
//
// Delegated to the Go binary (`autostart repair`) rather than reimplemented here:
// this must not grow a plist writer, a systemd unit writer and a schtasks caller
// in Node, three platforms out of sync with the Go ones. `repair` is a no-op when
// autostart was never enabled and never exits non-zero.
function repairAutostart(managed, log) {
  try {
    const r = spawnSync(managed, ["autostart", "repair"], {
      encoding: "utf8",
      timeout: 30_000,
    });
    const out = `${r.stdout || ""}${r.stderr || ""}`.trim();
    if (out) log(out);
  } catch {
    // Best-effort: a failed repair leaves autostart pointing at the old path,
    // which the engineer can fix with `promptster-teams autostart enable`.
  }
}

// installManagedBinary copies the bundled binary to the managed path when that
// is an upgrade, then repairs autostart. Returns a short status string for the
// caller's own reporting; never throws.
//
// log/warn are injected because the two callers need different streams:
// postinstall talks on stdout inside an npm install, while the launcher must
// stay on stderr so it cannot corrupt the output of the command being run.
// bundledOverride exists so the test suite can supply a real fixture binary:
// the actual bundled path resolves through an optionalDependency that is not
// installed in a source checkout, which would make every test take the
// "no binary to install" branch and prove nothing about the install itself.
function installManagedBinary({ log, warn, bundled: bundledOverride }) {
  // A project-local install must not touch the shared managed binary. The
  // lockfile is a per-PROJECT pin; the managed binary is per-USER. Letting a
  // local install write it means two repos pinning different versions fight over
  // one file and neither gets what its lockfile selected — a worse bug than the
  // npm-ls drift this whole design removes.
  if (!isGlobalInstall()) return "project-local";

  const bundled = bundledOverride || bundledBinPath();
  if (!bundled) {
    // Either an unsupported platform, or the optionalDependency carrying the
    // binary is absent. npm treats a missing optional dep as SUCCESS and says
    // nothing, so this warning is the only signal the engineer will ever get
    // that they have a CLI with no binary behind it. Say which package.
    if (!PLATFORMS.includes(platformKey())) {
      warn(`unsupported platform ${platformKey()} — no binary to install`);
      return "unsupported";
    }
    warn(`${platformPackage()} is not installed, so there is no binary to install.`);
    warn("If you used --omit=optional or --no-optional, reinstall without it.");
    return "no-bundled-binary";
  }
  const managed = managedBinPath();
  if (!managed) {
    warn("could not resolve home directory — skipping binary install");
    return "no-home";
  }

  const incoming = binVersion(bundled);
  const current = binVersion(managed);

  if (!incoming) {
    warn(`bundled binary at ${bundled} did not report a version — skipping`);
    return "unreadable-bundled";
  }

  // Never downgrade. The managed binary self-updates forward on its own, so it is
  // routinely NEWER than whatever version npm is installing (that is the normal
  // steady state, not an error). Clobbering it would hand the daemon an older
  // build that immediately re-updates — churn, plus a window on a version the
  // engineer already moved past. Mirrors the Go updater's isNewer gate.
  //
  // A managed binary that cannot report a version (current === null) is treated
  // as absent and overwritten: a corrupt or half-written file should be replaced,
  // not preserved by a guard meant to protect a GOOD newer build.
  if (current && !isNewer(incoming, current)) {
    // Record what we evaluated even though nothing was written, or the launcher's
    // cheap gate stays open and re-probes on every single invocation.
    writeMarker(incoming);
    return "already-current";
  }

  try {
    fs.mkdirSync(path.dirname(managed), { recursive: true });
    // Write to a temp file in the SAME directory, then rename: rename is atomic
    // on POSIX, so a concurrent `promptster-teams` exec sees either the whole old
    // binary or the whole new one, never a half-written file. Copying straight
    // onto `managed` would also fail with ETXTBSY on Linux if the daemon is
    // running. The pid in the name keeps two concurrent launchers apart.
    const tmp = `${managed}.tmp-${process.pid}`;
    fs.copyFileSync(bundled, tmp);
    fs.chmodSync(tmp, 0o755);

    // Re-check immediately before the swap. The guard above and this rename are
    // not atomic with respect to the Go self-updater, which renames onto this
    // same path from a live daemon: read 1.0.0 -> daemon installs 1.2.0 -> rename
    // 1.1.0 over it, and the guard has been defeated.
    //
    // This narrows the window from "the whole copy" to the microseconds around
    // the rename rather than closing it. Closing it needs a lock protocol shared
    // by a Node script and a Go daemon, which is a lot of machinery for this
    // failure: rename is atomic so the file is always ONE whole valid binary
    // (never corrupt), the only cost is running an older version, and the daemon
    // re-updates forward within one check interval (<=30m). Deliberate tradeoff,
    // not an oversight — see CLAUDE.md.
    const stillCurrent = binVersion(managed);
    if (stillCurrent && !isNewer(incoming, stillCurrent)) {
      fs.unlinkSync(tmp);
      writeMarker(incoming);
      log(`${managed} changed to ${stillCurrent} mid-install; leaving it`);
      return "raced";
    }
    fs.renameSync(tmp, managed);
    writeMarker(incoming);
    log(`installed ${incoming} to ${managed}${current ? ` (was ${current})` : ""}`);
    repairAutostart(managed, log);
    return "installed";
  } catch (err) {
    // Falls back to the bundled binary via bin/promptster-teams.js.
    warn(`could not install to ${managed}: ${err.message}`);
    warn("falling back to the bundled binary (npm ls may report a stale version)");
    return "failed";
  }
}

// shouldConvergeOnInvocation gates the launcher's self-heal.
//
// `uninstall` is the one command that must never trigger it: reinstalling the
// binary a person is in the middle of removing would leave them with a CLI they
// asked us to delete, and `uninstall` is already the ONLY working uninstall path
// (npm runs no uninstall lifecycle script — see CLAUDE.md), so undermining it
// leaves no other lever.
function shouldConvergeOnInvocation(subcommand) {
  return isGlobalInstall() && subcommand !== "uninstall";
}

module.exports = {
  installManagedBinary,
  mayNeedInstall,
  binVersion,
  shouldConvergeOnInvocation,
};
