#!/usr/bin/env node
// GitHub Action entry: squash-merge a ready PR when trusted checks and
// Greptile pass.
//
// Runs from MAIN's copy of this file (the workflow checkouts the default
// branch). A PR cannot weaken the policy by editing this script. Forks never
// merge — shouldAutomerge refuses them, and we never run PR code.
//
// Always exit 0 for "not yet" — a red Automerge check would deadlock merge if
// someone later adds it as a required status. Crashes (bugs) still fail.

import { readFileSync } from 'node:fs';

import { shouldAutomerge } from './automerge-decision.mjs';

const token = process.env.GITHUB_TOKEN;
const repository = process.env.GITHUB_REPOSITORY;
const eventName = process.env.GITHUB_EVENT_NAME ?? '';
const eventPath = process.env.GITHUB_EVENT_PATH;
const expectedBaseRef = process.env.AUTOMERGE_BASE_REF ?? 'main';

if (!token || !repository || !eventPath) {
  console.error('automerge: GITHUB_TOKEN, GITHUB_REPOSITORY, GITHUB_EVENT_PATH are required');
  process.exit(1);
}

const [owner, repo] = repository.split('/');
const event = JSON.parse(readFileSync(eventPath, 'utf8'));

const log = (msg, extra) => {
  console.log(extra ? `automerge: ${msg} ${JSON.stringify(extra)}` : `automerge: ${msg}`);
};

async function gh(path, { method = 'GET', body } = {}) {
  const res = await fetch(`https://api.github.com${path}`, {
    method,
    headers: {
      Authorization: `Bearer ${token}`,
      Accept: 'application/vnd.github+json',
      'X-GitHub-Api-Version': '2022-11-28',
      ...(body ? { 'Content-Type': 'application/json' } : {}),
    },
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  let json = null;
  try {
    json = text ? JSON.parse(text) : null;
  } catch {
    json = { raw: text };
  }
  return { status: res.status, json };
}

function headShaFromEvent() {
  if (event.pull_request?.head?.sha) return event.pull_request.head.sha;
  if (event.check_run?.head_sha) return event.check_run.head_sha;
  if (event.check_suite?.head_sha) return event.check_suite.head_sha;
  if (event.review?.commit_id) return event.review.commit_id;
  return null;
}

function prNumberFromEvent() {
  if (event.pull_request?.number) return event.pull_request.number;
  if (event.issue?.pull_request && event.issue?.number) return event.issue.number;
  return null;
}

async function prNumbers() {
  const n = prNumberFromEvent();
  if (n) return [n];
  const sha = headShaFromEvent();
  if (!sha) return [];
  const { status, json } = await gh(`/repos/${owner}/${repo}/commits/${sha}/pulls`);
  if (status !== 200 || !Array.isArray(json)) {
    log('could not list PRs for sha', { sha, status });
    return [];
  }
  return json.map((p) => p.number).filter(Boolean);
}

async function paginate(path) {
  const items = [];
  let page = 1;
  for (;;) {
    const sep = path.includes('?') ? '&' : '?';
    const { status, json } = await gh(`${path}${sep}per_page=100&page=${page}`);
    if (status !== 200) return { items, complete: false, status };
    const batch = Array.isArray(json) ? json : json.check_runs || json.files || [];
    items.push(...batch);
    if (batch.length < 100) return { items, complete: true, status };
    if (page >= 5) return { items, complete: false, status };
    page += 1;
  }
}

async function workflowPathFor(run) {
  const html = run.html_url || run.details_url || '';
  const m = String(html).match(/\/actions\/runs\/(\d+)/);
  if (!m) return null;
  const { status, json } = await gh(`/repos/${owner}/${repo}/actions/runs/${m[1]}`);
  if (status !== 200) return null;
  return json.path ?? null;
}

async function checkRunsFor(sha) {
  const { items, complete } = await paginate(`/repos/${owner}/${repo}/commits/${sha}/check-runs`);
  if (!complete) return { runs: items, complete: false };
  const mapped = [];
  for (const r of items) {
    const appSlug = r.app?.slug ?? null;
    let workflowPath = null;
    if (appSlug === 'github-actions') workflowPath = await workflowPathFor(r);
    mapped.push({
      name: r.name,
      status: r.status,
      conclusion: r.conclusion,
      startedAt: r.started_at,
      appSlug,
      workflowPath,
    });
  }
  return { runs: mapped, complete: true };
}

async function changedFiles(number) {
  const { items, complete } = await paginate(`/repos/${owner}/${repo}/pulls/${number}/files`);
  if (!complete) return { files: [], complete: false };
  return { files: items.map((f) => f.filename).filter(Boolean), complete: true };
}

// One pass over the PR's issue comments serves both gates: Greptile's summary
// and the verify-teams-cli attestation are both posted there.
async function commentEvidence(number) {
  const comments = await paginate(`/repos/${owner}/${repo}/issues/${number}/comments`);
  if (!comments.complete) {
    return { complete: false, summaryBody: null, attestations: [] };
  }
  const fromBot = (c) =>
    c.user?.login === 'greptile-apps[bot]' || c.user?.login === 'greptile-apps';
  const summaries = comments.items.filter(
    (c) => fromBot(c) && /<!--\s*greptile_summary\s*-->/i.test(c.body || ''),
  );
  // Greptile edits the summary in place; take the last matching issue comment.
  const summaryBody = summaries.length ? summaries[summaries.length - 1].body : null;
  // The attestation is not from the bot, so every comment goes to the
  // decision — but its AUTHOR goes with it. In a public repo anyone can
  // comment, so an attestation is only worth something when the person making
  // the claim is actually attached to the repository.
  const attestations = comments.items.map((c) => ({
    body: c.body || '',
    authorAssociation: c.author_association ?? null,
  }));
  return { complete: true, summaryBody, attestations };
}

async function consider(number) {
  const { status, json: pr } = await gh(`/repos/${owner}/${repo}/pulls/${number}`);
  if (status !== 200) {
    log('pr fetch failed', { number, status });
    return;
  }
  const fromFork = pr.head?.repo?.full_name !== pr.base?.repo?.full_name;
  const sha = pr.head?.sha;
  const [checks, files, comments] = await Promise.all([
    checkRunsFor(sha),
    changedFiles(number),
    commentEvidence(number),
  ]);
  if (!checks.complete) {
    log('skip', { number, reason: 'could not load a complete check-run list' });
    return;
  }
  if (!files.complete) {
    log('skip', { number, reason: 'could not load the PR file list' });
    return;
  }
  if (!comments.complete) {
    log('skip', { number, reason: 'could not load the PR comments' });
    return;
  }
  const decision = shouldAutomerge({
    isDraft: Boolean(pr.draft),
    state: pr.state,
    merged: Boolean(pr.merged),
    fromFork,
    baseRef: pr.base?.ref,
    expectedBaseRef,
    mergeable: pr.mergeable,
    checkRuns: checks.runs,
    changedFiles: files.files,
    greptileSummaryBody: comments.summaryBody,
    comments: comments.attestations,
    headSha: sha,
  });
  log('decision', { number, sha, ...decision, eventName });
  if (!decision.merge) return;

  const merge = await gh(`/repos/${owner}/${repo}/pulls/${number}/merge`, {
    method: 'PUT',
    body: {
      merge_method: 'squash',
      sha,
      commit_title: `${pr.title} (#${number})`,
    },
  });
  if (merge.status === 200) {
    log('merged', { number, sha });
    return;
  }
  log('merge api did not merge (will retry on next event)', {
    number,
    status: merge.status,
    message: merge.json?.message,
  });
}

const numbers = await prNumbers();
if (numbers.length === 0) {
  log('no pull requests for this event', { eventName });
  process.exit(0);
}
for (const n of numbers) {
  await consider(n);
}
