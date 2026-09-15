# Dependency-write capture pilot

Cursor's synchronous `beforeShellExecution` and `afterShellExecution` hooks now
capture changes to dependency lockfiles around direct npm, pnpm, and yarn install,
add, update, and remove commands. The generic command capture remains in place.

## What is recorded

The before hook snapshots lockfile SHA-256 hashes in the command's working
directory and its ancestors up to the repository root. The after hook records
only changed, regular files. A later commit receives AI attribution only when its
exact lockfile blob matches a recorded result and the commit is not older than
the recorded operation. Working-tree modification times are not used by this
new evidence path.

The commit event carries `files[].generationKind: "dependency"` and the agent
session ID. Its ranges use the existing `likely_ai` attribution, so they contribute
to AI-assisted line counts while remaining distinguishable from direct edits in
the captured event. The dashboard does not yet show a separate generated-lines
column. Both the CLI and backend projection preserve the category.

Hashes, commands used for pairing, and file contents are not sent in this new
metadata. The private `dependency-writes.json` ledger stores hashes and identities
on the device; no command text or file contents are persisted there.

## Boundaries

- Supported files: `package-lock.json`, `npm-shrinkwrap.json`, `pnpm-lock.yaml`,
  and `yarn.lock`, up to 16 MiB per file. New files are supported; deleted,
  unreadable, and symlinked files are not attributed by this path.
- Both live hooks must arrive with the same conversation, generation, command,
  and working directory. Pending pairs expire after 30 minutes; completed
  evidence expires after seven days.
- Overlapping observed shell invocations in a checkout invalidate the pair.
  Simultaneous edits by a terminal/editor outside the observed hooks cannot be
  ruled out, so the result remains `likely_ai`, not proof of exclusive authorship.
- Shell chains, directory overrides, global installs, and wrapper scripts are
  intentionally unsupported. Use a direct command with the intended working
  directory, for example `npm install` in that repository.
- Checkout changes cannot erase evidence for an already captured blob. A changed
  blob, including Git-filter/line-ending transformations, cannot inherit a hash
  that no longer matches.
- This pilot adds the before hook to the existing Cursor enrollment. Its response
  always includes `permission: "allow"`, including when capture fails or times
  out, and it performs no network I/O.
- Claude and Codex transcript capture does not provide a synchronous before
  callback today. This pilot does not infer their missing pre-command hashes.
- Existing direct-edit and legacy command-window attribution are unchanged.
  Historical unknown lines cannot be recovered from a new hash snapshot.

## Rollout and validation

Deploy the backend projection change before releasing the CLI so the dependency
category is retained from the first new event. Updating the enrolled CLI refreshes
Cursor hook registration through the existing enrollment path. No application or
hook installation is performed by the source changes alone.

Regression tests cover immutable commit matching, changed-content refusal,
missing/overlapping hooks, expiry, symlinks, monorepo root lockfiles, and repository
isolation. Backend projection tests assert that hashes and contents are stripped.

Hook reference: https://prod.cursor.com/docs/hooks
