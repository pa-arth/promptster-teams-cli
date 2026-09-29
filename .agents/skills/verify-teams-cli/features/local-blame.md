# Local incident origin

`blame-backfill --repo <local-checkout> --limit <1..500> [--dry-run]` walks recent first-parent commits. Dry-run prints only SHA/count/workspace metadata without credentials or disk writes. Queueing requires login and writes signed `commit_line_origin` events to the backfill outbox for the existing capture daemon.

Drive dry-run against a disposable Git repository with one introduced line and a later fix; assert exact origin SHA/count and absence of source, filename, author and message. Then use sandbox `login` and run without dry-run. Inspect the buffer and backfill outbox: both retain the origin metadata, and neither carries source. Replay duplicates are selected rather than summed by the backend. Local shallow history and binary files must be unavailable/partial, never observed zero.

The end-to-end API proof is backend `scripts/incident-origin-local-proof.mts`. It feeds actual CLI dry-run output to real ingest, exercises worker derivation and reads manager output; fixture identity/GitHub metadata responses never permit server blame or Commit/Contents requests.
