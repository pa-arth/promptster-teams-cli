import { test } from 'node:test';
import assert from 'node:assert/strict';

import {
  GREPTILE_MIN_CONFIDENCE,
  PRIVILEGED_PATHS,
  REQUIRED_CHECK_NAMES,
  REQUIRED_CHECKS,
  TRUSTED_ATTESTER_ASSOCIATIONS,
  VERIFICATION_MARKER,
  greptileBlocksMerge,
  shouldAutomerge,
  verificationBlocksMerge,
} from '../automerge-decision.mjs';

const HEAD_SHA = 'f5eabde6ebc60d622fdc963031d1c74310b28e46';
const attested = (sha = HEAD_SHA, authorAssociation = 'OWNER') => [
  { body: `<!-- verified: verify-teams-cli sha=${sha} -->\nDrove it.`, authorAssociation },
];
const said = (body, authorAssociation = 'OWNER') => ({ body, authorAssociation });

// The check name + workflow path a spoofing PR would most want to imitate.
const CI_NAME = 'race & coverage';
const CI_PATH = '.github/workflows/ci.yml';

function trustedSuccess() {
  return REQUIRED_CHECKS.map((spec, i) => ({
    name: spec.name,
    appSlug: spec.appSlug,
    workflowPath: spec.workflowPath,
    status: 'completed',
    conclusion: 'success',
    startedAt: new Date(Date.parse('2026-09-16T21:00:00Z') + i * 60_000).toISOString(),
  }));
}

const CLEAN_GREPTILE = {
  greptileSummaryBody: '<!-- greptile_summary -->\n<h2>Confidence Score: 5/5</h2>\nLooks good.',
  greptileReviewComments: [],
};

function decide(overrides = {}) {
  return shouldAutomerge({
    isDraft: false,
    state: 'open',
    merged: false,
    fromFork: false,
    baseRef: 'main',
    expectedBaseRef: 'main',
    mergeable: true,
    checkRuns: trustedSuccess(),
    changedFiles: ['README.md'],
    headSha: HEAD_SHA,
    comments: attested(),
    ...CLEAN_GREPTILE,
    ...overrides,
  });
}

test('merges a ready same-repo PR once trusted checks and Greptile pass', () => {
  const r = decide();
  assert.equal(r.merge, true, r.reason);
});

test('refuses a draft even when CI and Greptile are green', () => {
  const r = decide({ isDraft: true });
  assert.equal(r.merge, false);
  assert.match(r.reason, /draft/);
});

test('refuses a fork PR even when it is ready and green', () => {
  const r = decide({ fromFork: true });
  assert.equal(r.merge, false);
  assert.match(r.reason, /fork/);
});

test('refuses until every required check has reported from the trusted app', () => {
  const r = decide({ checkRuns: trustedSuccess().slice(0, -1) });
  assert.equal(r.merge, false);
  assert.match(r.reason, /has not reported/);
  assert.ok(r.reason.includes('Greptile Review'), r.reason);
});

test('a same-name success from the wrong app does not authorize a merge', () => {
  const runs = trustedSuccess().map((r) =>
    r.name === CI_NAME ? { ...r, appSlug: 'malicious-app' } : r,
  );
  const r = decide({ checkRuns: runs });
  assert.equal(r.merge, false);
  assert.match(r.reason, /github-actions/);
});

test('a same-name success from a different workflow file does not count', () => {
  const runs = trustedSuccess().map((r) =>
    r.name === CI_NAME ? { ...r, workflowPath: '.github/workflows/looks-green.yml' } : r,
  );
  const r = decide({ checkRuns: runs });
  assert.equal(r.merge, false);
  assert.match(r.reason, /ci\.yml/);
});

test('a later spoofed same-name run does not beat the trusted one', () => {
  const trusted = trustedSuccess();
  const spoof = {
    name: CI_NAME,
    appSlug: 'github-actions',
    workflowPath: '.github/workflows/looks-green.yml',
    status: 'completed',
    conclusion: 'success',
    startedAt: '2026-09-16T23:00:00Z',
  };
  const r = decide({ checkRuns: [...trusted, spoof] });
  assert.equal(r.merge, true, r.reason);
});

test('refuses when the PR edits a privileged workflow or automerge script', () => {
  const r = decide({ changedFiles: ['.github/workflows/codeql.yml', 'README.md'] });
  assert.equal(r.merge, false);
  assert.match(r.reason, /privileged path/);
  assert.ok(r.reason.includes('.github/workflows/codeql.yml'), r.reason);
});

test('every privileged path really blocks, one at a time', () => {
  for (const p of PRIVILEGED_PATHS) {
    const r = decide({ changedFiles: [p] });
    assert.equal(r.merge, false, p);
    assert.match(r.reason, /privileged path/);
  }
});

test('refuses a required check that is still in progress', () => {
  const runs = trustedSuccess();
  runs[runs.length - 1] = { ...runs[runs.length - 1], status: 'in_progress', conclusion: null };
  const r = decide({ checkRuns: runs });
  assert.equal(r.merge, false);
  assert.match(r.reason, /in_progress/);
});

test('refuses a required check that failed', () => {
  const runs = trustedSuccess();
  runs[runs.length - 1] = { ...runs[runs.length - 1], conclusion: 'failure' };
  const r = decide({ checkRuns: runs });
  assert.equal(r.merge, false);
  assert.match(r.reason, /failure/);
});

test('a later successful trusted rerun beats an older trusted failure', () => {
  const runs = trustedSuccess();
  const failed = {
    ...runs[runs.length - 1],
    conclusion: 'failure',
    startedAt: '2026-09-16T20:00:00Z',
  };
  const r = decide({ checkRuns: [...runs, failed] });
  assert.equal(r.merge, true, r.reason);
});

test('an unrelated failing check does not block', () => {
  const r = decide({
    checkRuns: [
      ...trustedSuccess(),
      {
        name: 'Lint the docs nobody reads',
        appSlug: 'github-actions',
        workflowPath: CI_PATH,
        status: 'completed',
        conclusion: 'failure',
        startedAt: '2026-09-16T21:11:00Z',
      },
    ],
  });
  assert.equal(r.merge, true, r.reason);
});

test('refuses a PR that is not targeting main', () => {
  const r = decide({ baseRef: 'staging' });
  assert.equal(r.merge, false);
  assert.match(r.reason, /staging/);
});

test('refuses when GitHub says the PR is not mergeable', () => {
  const r = decide({ mergeable: false });
  assert.equal(r.merge, false);
  assert.match(r.reason, /not mergeable/);
});

test('null mergeable does not block a green ready PR', () => {
  const r = decide({ mergeable: null });
  assert.equal(r.merge, true, r.reason);
});

test('refuses closed and already-merged PRs', () => {
  assert.equal(decide({ state: 'closed' }).merge, false);
  assert.equal(decide({ merged: true }).merge, false);
});

test('Greptile check success is not enough without a summary', () => {
  const r = decide({ greptileSummaryBody: null });
  assert.equal(r.merge, false);
  assert.match(r.reason, /summary/);
});

test('Greptile confidence below the floor blocks merge', () => {
  const r = decide({
    greptileSummaryBody:
      '<!-- greptile_summary -->\n<h2>Confidence Score: 1/5</h2>\nThis PR is not safe to merge until X.',
  });
  assert.equal(r.merge, false);
  assert.match(r.reason, /1\/5/);
});

test('Greptile P1s in the current summary block merge even at 5/5', () => {
  const r = decide({
    greptileSummaryBody:
      '<!-- greptile_summary -->\n<h2>Confidence Score: 5/5</h2>\n<img alt="P1" src="https://greptile-static-assets.s3.amazonaws.com/badges/p1.svg?v=9"> hole',
  });
  assert.equal(r.merge, false);
  assert.match(r.reason, /P1/);
});

test('historical inline P1s do not block once the current summary is clean', () => {
  const r = decide({
    greptileSummaryBody: '<!-- greptile_summary -->\n<h2>Confidence Score: 5/5</h2>\nLooks good.',
    greptileReviewComments: [
      {
        body: '<img alt="P1" src="https://greptile-static-assets.s3.amazonaws.com/badges/p1.svg?v=9"> already fixed',
      },
    ],
  });
  assert.equal(r.merge, true, r.reason);
});

test('a low-confidence summary with a P1 does not merge', () => {
  const r = decide({
    greptileSummaryBody: `<!-- greptile_summary -->
<h2>Confidence Score: 1/5</h2>
This PR is not safe to merge until automerge authenticates the required checks.
<img alt="P1" src="https://greptile-static-assets.s3.amazonaws.com/badges/p1.svg?v=9">
`,
  });
  assert.equal(r.merge, false);
  assert.match(r.reason, /1\/5|P1|not safe/i);
});

test('Greptile P2 comments do not block at 5/5', () => {
  const r = decide({
    greptileSummaryBody: '<!-- greptile_summary -->\n<h2>Confidence Score: 5/5</h2>\nFine.',
    greptileReviewComments: [
      {
        body: '<img alt="P2" src="https://greptile-static-assets.s3.amazonaws.com/badges/p2.svg?v=9"> nit',
      },
    ],
  });
  assert.equal(r.merge, true, r.reason);
});

test('Greptile 4/5 does not merge — the floor is 5/5', () => {
  const r = decide({
    greptileSummaryBody: '<!-- greptile_summary -->\n<h2>Confidence Score: 4/5</h2>\nMostly fine.',
  });
  assert.equal(r.merge, false);
  assert.match(r.reason, /4\/5 is below 5\/5/);
});

test('refuses a PR with no verify-teams-cli attestation', () => {
  const r = decide({ comments: [said('nice work'), said('<!-- greptile_summary --> ...')] });
  assert.equal(r.merge, false);
  assert.match(r.reason, /no verify-teams-cli attestation/);
});

test('refuses an attestation for an earlier commit — verify A, push B', () => {
  const r = decide({ comments: attested('678d8411026e5d58cb43ea3bb8a91adc546ddafb') });
  assert.equal(r.merge, false);
  assert.match(r.reason, /re-verify after the last push/);
});

test('accepts an abbreviated SHA that prefixes the real head', () => {
  const r = decide({ comments: attested(HEAD_SHA.slice(0, 7)) });
  assert.equal(r.merge, true, r.reason);
});

test('an abbreviated SHA that is not a prefix of head does not count', () => {
  const r = decide({ comments: attested('deadbee') });
  assert.equal(r.merge, false);
  assert.match(r.reason, /not head/);
});

test('the attestation is found among many unrelated comments', () => {
  const r = decide({
    comments: [said('lgtm'), ...attested(), said('one more thought')],
  });
  assert.equal(r.merge, true, r.reason);
});

test('a sibling repo attestation does not satisfy this gate', () => {
  const r = decide({
    comments: [said(`<!-- verified: verify-backend sha=${HEAD_SHA} -->`)],
  });
  assert.equal(r.merge, false);
  assert.match(r.reason, /no verify-teams-cli attestation/);
});

test('an attestation from a stranger does not authorize a merge', () => {
  // Three of these repos are PUBLIC. Anyone with a GitHub account can comment
  // on a PR, so the marker alone is forgeable; the author is what makes it a
  // claim by someone attached to the repository.
  const r = decide({ comments: attested(HEAD_SHA, 'NONE') });
  assert.equal(r.merge, false);
  assert.match(r.reason, /untrusted author/);
});

test('CONTRIBUTOR is not enough to attest', () => {
  // "has had a PR merged here once" is not "is trusted now".
  const r = decide({ comments: attested(HEAD_SHA, 'CONTRIBUTOR') });
  assert.equal(r.merge, false);
  assert.match(r.reason, /untrusted author/);
});

test('an attestation with no author association at all fails closed', () => {
  const r = decide({ comments: [{ body: attested()[0].body }] });
  assert.equal(r.merge, false);
  assert.match(r.reason, /untrusted author/);
});

test('every trusted association may attest', () => {
  for (const assoc of TRUSTED_ATTESTER_ASSOCIATIONS) {
    const r = decide({ comments: attested(HEAD_SHA, assoc) });
    assert.equal(r.merge, true, `${assoc}: ${r.reason}`);
  }
});

test('a trusted attestation still counts alongside a forged one', () => {
  const r = decide({
    comments: [...attested(HEAD_SHA, 'NONE'), ...attested(HEAD_SHA, 'OWNER')],
  });
  assert.equal(r.merge, true, r.reason);
});

test('a stranger cannot pre-empt the real attestation with a wrong SHA', () => {
  const r = decide({ comments: attested('deadbee', 'NONE') });
  assert.equal(r.merge, false);
  assert.match(r.reason, /untrusted author/);
});

test('verificationBlocksMerge fails closed with no head SHA', () => {
  const reason = verificationBlocksMerge({ comments: attested(), headSha: null });
  assert.match(reason, /fail closed/);
});

test('the marker regex is not global — repeated exec must not skip', () => {
  // A /g regex carries lastIndex between calls and would silently miss the
  // second comment in a PR. Guard the flag, not just the behaviour.
  assert.equal(VERIFICATION_MARKER.global, false);
  const body = attested()[0].body;
  assert.ok(VERIFICATION_MARKER.exec(body));
  assert.ok(VERIFICATION_MARKER.exec(body));
});

test('greptileBlocksMerge fails closed without a score', () => {
  const reason = greptileBlocksMerge({
    summaryBody: '<!-- greptile_summary --> we looked at it',
    reviewComments: [],
  });
  assert.match(reason, /Confidence Score/);
});

// The gate this repo is porting away from is a required check that NEVER
// reports: fail-closed then means never merge. Every name below was read off a
// live check-run list first; this test is what keeps it that way when a job is
// renamed.
test('REQUIRED_CHECKS stay in lockstep with this repo\'s real workflow jobs', async () => {
  const { readFileSync } = await import('node:fs');
  const wf = (p) => readFileSync(new URL(`../../${p}`, import.meta.url), 'utf8');

  const wf_ci_yml = wf('.github/workflows/ci.yml');
  const wf_codeql_yml = wf('.github/workflows/codeql.yml');
  const wf_secret_scan_yml = wf('.github/workflows/secret-scan.yml');
  const wf_verify_map_yml = wf('.github/workflows/verify-map.yml');

  assert.match(wf_ci_yml, /name: build & test \(\$\{\{ matrix\.os \}\}\)/);
  assert.match(wf_ci_yml, /os: \[ubuntu-latest, macos-latest\]/);
  assert.match(wf_ci_yml, /name: autostart smoke \(\$\{\{ matrix\.os \}\}\)/);
  assert.match(wf_ci_yml, /os: \[ubuntu-latest, windows-latest\]/);
  assert.match(wf_ci_yml, /name: race & coverage/);
  assert.match(wf_ci_yml, /name: lint \(gofmt, vet, staticcheck\)/);
  assert.match(wf_ci_yml, /name: npm platform lists in lockstep/);
  assert.match(wf_ci_yml, /name: gosec \(SAST\)/);
  assert.match(wf_ci_yml, /name: govulncheck \(deps \+ stdlib CVEs\)/);
  assert.match(wf_ci_yml, /name: installer signature verification/);
  assert.match(wf_ci_yml, /name: progress-schema migrations declare their replay cost/);
  assert.match(wf_ci_yml, /name: audit \(no surveillance\/anti-cheat logic\)/);
  assert.match(wf_codeql_yml, /name: CodeQL analyze \(go\)/);
  assert.match(wf_secret_scan_yml, /name: gitleaks/);
  assert.match(wf_verify_map_yml, /name: Verification map anchors/);

  assert.deepEqual(REQUIRED_CHECK_NAMES, [
    'build & test (ubuntu-latest)',
    'build & test (macos-latest)',
    'autostart smoke (ubuntu-latest)',
    'autostart smoke (windows-latest)',
    'race & coverage',
    'lint (gofmt, vet, staticcheck)',
    'npm platform lists in lockstep',
    'gosec (SAST)',
    'govulncheck (deps + stdlib CVEs)',
    'installer signature verification',
    'progress-schema migrations declare their replay cost',
    'audit (no surveillance/anti-cheat logic)',
    'CodeQL analyze (go)',
    'gitleaks',
    'Verification map anchors',
    'Greptile Review',
  ]);

  assert.equal(REQUIRED_CHECKS[0].appSlug, 'github-actions');
  assert.equal(REQUIRED_CHECKS[0].workflowPath, '.github/workflows/ci.yml');
  assert.equal(REQUIRED_CHECKS[1].appSlug, 'github-actions');
  assert.equal(REQUIRED_CHECKS[1].workflowPath, '.github/workflows/ci.yml');
  assert.equal(REQUIRED_CHECKS[2].appSlug, 'github-actions');
  assert.equal(REQUIRED_CHECKS[2].workflowPath, '.github/workflows/ci.yml');
  assert.equal(REQUIRED_CHECKS[3].appSlug, 'github-actions');
  assert.equal(REQUIRED_CHECKS[3].workflowPath, '.github/workflows/ci.yml');
  assert.equal(REQUIRED_CHECKS[4].appSlug, 'github-actions');
  assert.equal(REQUIRED_CHECKS[4].workflowPath, '.github/workflows/ci.yml');
  assert.equal(REQUIRED_CHECKS[5].appSlug, 'github-actions');
  assert.equal(REQUIRED_CHECKS[5].workflowPath, '.github/workflows/ci.yml');
  assert.equal(REQUIRED_CHECKS[6].appSlug, 'github-actions');
  assert.equal(REQUIRED_CHECKS[6].workflowPath, '.github/workflows/ci.yml');
  assert.equal(REQUIRED_CHECKS[7].appSlug, 'github-actions');
  assert.equal(REQUIRED_CHECKS[7].workflowPath, '.github/workflows/ci.yml');
  assert.equal(REQUIRED_CHECKS[8].appSlug, 'github-actions');
  assert.equal(REQUIRED_CHECKS[8].workflowPath, '.github/workflows/ci.yml');
  assert.equal(REQUIRED_CHECKS[9].appSlug, 'github-actions');
  assert.equal(REQUIRED_CHECKS[9].workflowPath, '.github/workflows/ci.yml');
  assert.equal(REQUIRED_CHECKS[10].appSlug, 'github-actions');
  assert.equal(REQUIRED_CHECKS[10].workflowPath, '.github/workflows/ci.yml');
  assert.equal(REQUIRED_CHECKS[11].appSlug, 'github-actions');
  assert.equal(REQUIRED_CHECKS[11].workflowPath, '.github/workflows/ci.yml');
  assert.equal(REQUIRED_CHECKS[12].appSlug, 'github-actions');
  assert.equal(REQUIRED_CHECKS[12].workflowPath, '.github/workflows/codeql.yml');
  assert.equal(REQUIRED_CHECKS[13].appSlug, 'github-actions');
  assert.equal(REQUIRED_CHECKS[13].workflowPath, '.github/workflows/secret-scan.yml');
  assert.equal(REQUIRED_CHECKS[14].appSlug, 'github-actions');
  assert.equal(REQUIRED_CHECKS[14].workflowPath, '.github/workflows/verify-map.yml');
  assert.equal(REQUIRED_CHECKS[15].appSlug, 'greptile-apps');
  assert.equal(REQUIRED_CHECKS[15].workflowPath, undefined);

  assert.equal(GREPTILE_MIN_CONFIDENCE, 5);
  assert.deepEqual(TRUSTED_ATTESTER_ASSOCIATIONS, ['OWNER', 'MEMBER', 'COLLABORATOR']);

  // The decision test has to actually run inside a check the gate requires,
  // or these assertions never fire where it matters.
  assert.match(wf_ci_yml, /automerge-decision\.test\.mjs/);

  for (const p of [
    '.github/workflows/automerge.yml',
    '.github/workflows/ci.yml',
    '.github/workflows/codeql.yml',
    '.github/workflows/secret-scan.yml',
    '.github/workflows/verify-map.yml',
    'scripts/automerge.mjs',
    'scripts/automerge-decision.mjs',
  ]) {
    assert.ok(PRIVILEGED_PATHS.includes(p), p);
  }
  // Every workflow that produces a required github-actions check must be
  // privileged, or a PR can rewrite the job that authorizes it.
  for (const spec of REQUIRED_CHECKS) {
    if (!spec.workflowPath) continue;
    assert.ok(PRIVILEGED_PATHS.includes(spec.workflowPath), spec.workflowPath);
  }
});

test("the automerge workflow runs main's copy of the script, not the PR's", async () => {
  const { readFileSync } = await import('node:fs');
  const wf = readFileSync(
    new URL('../../.github/workflows/automerge.yml', import.meta.url),
    'utf8',
  );
  assert.match(wf, /^on:\n(?:  .*\n)*  pull_request_target:/m);
  assert.equal(
    wf.includes('ref: ${{ github.event.repository.default_branch }}'),
    true,
    'must checkout the default branch, never the PR head',
  );
  assert.equal(wf.includes('github.event.pull_request.head.sha'), false);
  assert.match(
    wf,
    /github\.event\.check_run\.name != 'Automerge on green'/,
    'must ignore its own check_run or it retriggers forever',
  );
  assert.match(wf, /issue_comment:/, 'must retry after Greptile posts its summary');
  // main has the workflow before it has the script, for exactly as long as the
  // introducing PR is open. Without this guard the gate goes red on that PR.
  assert.match(
    wf,
    /if \[ ! -f scripts\/automerge\.mjs \]; then/,
    'must no-op when the default branch does not have the script yet',
  );
  // Without this the gate deadlocks: once CI and Greptile have finished there
  // is no event left to fire, so the attestation the decision is waiting for
  // would be posted and never read.
  assert.match(
    wf,
    /contains\(github\.event\.comment\.body, 'verified: verify-teams-cli'\)/,
    'an attestation comment must retrigger the merge decision',
  );
  assert.match(wf, /AUTOMERGE_BASE_REF: main/, 'this repo\'s default branch is main');
});
