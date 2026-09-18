#!/usr/bin/env node
// Should this PR squash-merge without a human?
//
// Cursor Cloud Agents open a DRAFT PR before they finish testing (they have to:
// the environment asks them to push a pre-testing revision). CI can go green on
// that draft while the agent is still proving the change against the scratch
// stack. Auto-merging on CI alone would land unverified agent work.
//
// Draft is the verification gate. Ready-for-review means the agent (or a human)
// ran verify-teams-cli. That is still a self-attestation — it is not a substitute
// for CI or for Greptile. GitHub then squash-merges only when:
//
//   1. The named CI jobs are green AND came from github-actions running the
//      expected workflow PATHS (not a same-name job in a PR-added workflow).
//   2. Those workflow files were not edited in the PR — otherwise the PR can
//      gut ci.yml, keep the job names, and present a trivial success.
//   3. Greptile has finished a pass on this PR, at 5/5 — or at 4/5 with no P1
//      ever raised, which is a P2-only PR. Greptile's check-run conclusion is
//      "we reviewed", not "safe to merge": it reports success even at 1/5.
//   4. A verify-teams-cli attestation from a trusted author names THIS head SHA.
//      Ready-for-review was only ever a self-attestation that nothing read;
//      this makes it a machine-checked one. It does not prove the agent drove the app — it
//      cannot — but it does turn a silent omission into an explicit claim,
//      and it kills the stale-proof case: verify commit A, push commit B, and
//      the attestation no longer matches the head SHA.
//
// Fail closed: a missing name, a missing app, a missing Greptile summary, a
// missing or stale verification attestation, or a privileged-path edit does
// not merge.

export const REQUIRED_CHECKS = [
  {
    name: 'build & test (ubuntu-latest)',
    appSlug: 'github-actions',
    workflowPath: '.github/workflows/ci.yml',
  },
  {
    name: 'build & test (macos-latest)',
    appSlug: 'github-actions',
    workflowPath: '.github/workflows/ci.yml',
  },
  {
    name: 'autostart smoke (ubuntu-latest)',
    appSlug: 'github-actions',
    workflowPath: '.github/workflows/ci.yml',
  },
  {
    name: 'autostart smoke (windows-latest)',
    appSlug: 'github-actions',
    workflowPath: '.github/workflows/ci.yml',
  },
  {
    name: 'race & coverage',
    appSlug: 'github-actions',
    workflowPath: '.github/workflows/ci.yml',
  },
  {
    name: 'lint (gofmt, vet, staticcheck)',
    appSlug: 'github-actions',
    workflowPath: '.github/workflows/ci.yml',
  },
  {
    name: 'npm platform lists in lockstep',
    appSlug: 'github-actions',
    workflowPath: '.github/workflows/ci.yml',
  },
  {
    name: 'gosec (SAST)',
    appSlug: 'github-actions',
    workflowPath: '.github/workflows/ci.yml',
  },
  {
    name: 'govulncheck (deps + stdlib CVEs)',
    appSlug: 'github-actions',
    workflowPath: '.github/workflows/ci.yml',
  },
  {
    name: 'installer signature verification',
    appSlug: 'github-actions',
    workflowPath: '.github/workflows/ci.yml',
  },
  {
    name: 'progress-schema migrations declare their replay cost',
    appSlug: 'github-actions',
    workflowPath: '.github/workflows/ci.yml',
  },
  {
    name: 'audit (no surveillance/anti-cheat logic)',
    appSlug: 'github-actions',
    workflowPath: '.github/workflows/ci.yml',
  },
  {
    name: 'CodeQL analyze (go)',
    appSlug: 'github-actions',
    workflowPath: '.github/workflows/codeql.yml',
  },
  {
    name: 'gitleaks',
    appSlug: 'github-actions',
    workflowPath: '.github/workflows/secret-scan.yml',
  },
  {
    name: 'Verification map anchors',
    appSlug: 'github-actions',
    workflowPath: '.github/workflows/verify-map.yml',
  },
  {
    name: 'Greptile Review',
    appSlug: 'greptile-apps',
  },
];

export const REQUIRED_CHECK_NAMES = REQUIRED_CHECKS.map((c) => c.name);

// A PR that edits these is changing the merge policy or the checks that
// authorize it. Automerge must not land that class of change.
export const PRIVILEGED_PATHS = [
  '.github/workflows/automerge.yml',
  '.github/workflows/ci.yml',
  '.github/workflows/codeql.yml',
  '.github/workflows/secret-scan.yml',
  '.github/workflows/verify-map.yml',
  'scripts/automerge.mjs',
  'scripts/automerge-decision.mjs',
];

export const GREPTILE_MIN_CONFIDENCE = 5;

// A 4/5 with no P1 ever raised is a P2-only PR. Measured over 40 PRs: every
// single 4/5 had at least one finding, and 10 of 21 were P2-only — the
// severity Greptile itself calls non-blocking. A flat 5/5 floor blocks a doc
// nit exactly as hard as a correctness bug.
export const GREPTILE_P2_ONLY_CONFIDENCE = 4;

// Who is allowed to attest. GitHub's author_association on a PR comment.
// THIS REPO IS PUBLIC: anyone with a GitHub account can comment on a PR here,
// so without this check the proof-of-drive claim is forgeable by a stranger
// today — not hypothetically. (promptster-backend, where this gate came from,
// is private, so the same code is latent there; the hole is in the DESIGN, not
// the repo.) CONTRIBUTOR is deliberately absent: it means "has had a PR merged
// here before", not "is trusted now".
export const TRUSTED_ATTESTER_ASSOCIATIONS = ['OWNER', 'MEMBER', 'COLLABORATOR'];

// An agent posts this as a PR comment after it has driven verify-teams-cli.
// The SHA is what makes it worth anything: it has to name the commit being
// merged, so a proof cannot be inherited by a later push.
//
//   gh pr comment <N> --body "<!-- verified: verify-teams-cli sha=$(git rev-parse HEAD) -->"
export const VERIFICATION_MARKER =
  /<!--\s*verified:\s*verify-teams-cli\s+sha=([0-9a-f]{7,40})\s*-->/i;

/**
 * @param {object} input
 * @returns {{ merge: boolean, reason: string }}
 */
export function shouldAutomerge(input) {
  const required = input.requiredChecks ?? REQUIRED_CHECKS;

  if (input.fromFork) return { merge: false, reason: 'fork PR — never auto-merge untrusted code' };
  if (input.state !== 'open') return { merge: false, reason: `state is ${input.state}` };
  if (input.merged) return { merge: false, reason: 'already merged' };
  if (input.isDraft) {
    return {
      merge: false,
      reason: 'draft — verification gate; mark ready only after verify-teams-cli proof',
    };
  }
  if (input.baseRef !== input.expectedBaseRef) {
    return { merge: false, reason: `base is ${input.baseRef}, not ${input.expectedBaseRef}` };
  }
  if (input.mergeable === false) {
    return { merge: false, reason: 'GitHub says not mergeable (conflicts or blocked)' };
  }

  const privilegedHit = (input.changedFiles ?? []).filter((f) =>
    (input.privilegedPaths ?? PRIVILEGED_PATHS).includes(f),
  );
  if (privilegedHit.length) {
    return {
      merge: false,
      reason: `privileged path changed (${privilegedHit.join(', ')}) — human merge required`,
    };
  }

  for (const spec of required) {
    const run = latestMatchingRun(input.checkRuns ?? [], spec);
    if (!run) {
      return {
        merge: false,
        reason: `required check has not reported from ${spec.appSlug}${spec.workflowPath ? ` (${spec.workflowPath})` : ''}: ${spec.name}`,
      };
    }
    if (run.status !== 'completed') {
      return { merge: false, reason: `required check still ${run.status}: ${spec.name}` };
    }
    if (run.conclusion !== 'success') {
      return { merge: false, reason: `required check ${run.conclusion}: ${spec.name}` };
    }
  }

  const greptileBlock = greptileBlocksMerge({
    summaryBody: input.greptileSummaryBody,
    minConfidence: input.greptileMinConfidence,
  });
  if (greptileBlock) return { merge: false, reason: greptileBlock };

  const verificationBlock = verificationBlocksMerge({
    comments: input.comments,
    headSha: input.headSha,
  });
  if (verificationBlock) return { merge: false, reason: verificationBlock };

  return {
    merge: true,
    reason:
      'ready, trusted checks green, Greptile pass, verify-teams-cli attested for this SHA, no privileged-path edits',
  };
}

export function checkMatchesSpec(run, spec) {
  if (!run || run.name !== spec.name) return false;
  if (run.appSlug !== spec.appSlug) return false;
  if (spec.workflowPath && run.workflowPath !== spec.workflowPath) return false;
  return true;
}

export function latestMatchingRun(checkRuns, spec) {
  let best = null;
  for (const run of checkRuns) {
    if (!checkMatchesSpec(run, spec)) continue;
    if (!best || startedMs(run) >= startedMs(best)) best = run;
  }
  return best;
}

export function greptileBlocksMerge({ summaryBody, minConfidence = GREPTILE_MIN_CONFIDENCE } = {}) {
  // Which fields here are CURRENT, measured over 40 PRs of history:
  //
  //   Confidence Score  recomputed every review. Trustworthy.
  //   Findings list     CUMULATIVE. Resolved findings keep their badge, so a
  //                     5/5 summary reading "both findings are fully
  //                     addressed" still carries a P1 image. Presence of a
  //                     badge proves nothing about now.
  //
  // The asymmetry is what makes the badge usable at all: it only ever
  // accumulates, so ABSENCE is reliable (that severity was never raised) even
  // though presence is not. So: trust the score, and read the badge in the
  // one direction the data supports.
  if (!summaryBody || !/<!--\s*greptile_summary\s*-->/i.test(summaryBody)) {
    return 'Greptile has not posted a review summary yet';
  }
  const m = summaryBody.match(/Confidence Score:\s*(\d+)\s*\/\s*5/i);
  if (!m) return 'Greptile summary has no Confidence Score — fail closed';
  const score = Number(m[1]);

  // Narrative verdict overrides the number in the one direction that fails
  // closed. Greptile writes this sentence per review, so it is current.
  if (/not (?:yet )?safe to merge/i.test(summaryBody)) {
    return 'Greptile summary says not safe to merge';
  }

  if (score >= minConfidence) return null;

  // One below the floor with no P1 ever raised: P2-only, merge it.
  if (score === GREPTILE_P2_ONLY_CONFIDENCE && !/badges\/p1\.svg/i.test(summaryBody)) {
    return null;
  }
  if (score === GREPTILE_P2_ONLY_CONFIDENCE) {
    return `Greptile confidence ${score}/5 and a P1 has been raised on this PR`;
  }
  return `Greptile confidence ${score}/5 is below ${minConfidence}/5`;
}

export function verificationBlocksMerge({
  comments,
  headSha,
  trustedAssociations = TRUSTED_ATTESTER_ASSOCIATIONS,
} = {}) {
  if (!headSha) return 'no head SHA to check a verification attestation against — fail closed';
  const claimed = [];
  let untrusted = 0;
  for (const comment of comments ?? []) {
    const m = VERIFICATION_MARKER.exec(comment?.body || '');
    if (!m) continue;
    // Association is checked BEFORE the SHA, so a stranger posting the right
    // SHA is still a stranger. An entry with no association at all is
    // untrusted: fail closed rather than trusting a field the caller forgot.
    if (!trustedAssociations.includes(comment?.authorAssociation)) {
      untrusted += 1;
      continue;
    }
    claimed.push(m[1].toLowerCase());
  }
  if (!claimed.length) {
    if (untrusted) {
      return `the only verify-teams-cli attestation on this PR is from an untrusted author — anyone can comment, so only ${trustedAssociations.join('/')} counts`;
    }
    return 'no verify-teams-cli attestation on this PR — run verify-teams-cli and post one';
  }
  const head = headSha.toLowerCase();
  // Accept an abbreviated SHA, but only as a prefix of the real head. `git
  // rev-parse HEAD` gives 40; a human pasting 7 should not be punished for it.
  const matched = claimed.some((sha) => head.startsWith(sha));
  if (!matched) {
    return `verify-teams-cli attestation is for ${claimed.join(', ')}, not head ${head.slice(0, 8)} — re-verify after the last push`;
  }
  return null;
}

function startedMs(run) {
  if (!run.startedAt) return 0;
  const t = Date.parse(run.startedAt);
  return Number.isFinite(t) ? t : 0;
}
