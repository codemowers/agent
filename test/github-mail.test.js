import test from 'node:test';
import assert from 'node:assert/strict';
import {githubMessageReference, githubNotificationReference, resolveGithubMailIssues, routeGithubIssueMail} from '../src/github-mail.js';
import {githubTask} from '../src/github.js';
import {changeState, digest} from '../src/domain.js';

const messageId = '<codemowers/kube/issue/122/issue_event/33045581906@github.com>';
const issue = {repository: 'codemowers/kube', state: 'open', assignees: ['agent-smith'], title: 'Implement feature', content: 'Implement feature\n\nPrivate issue specification', updatedAt: '2026-10-12T16:00:00Z',
  reference: {source: 'github', kind: 'issue', repository: 'codemowers/kube', id: '122', url: 'https://github.com/codemowers/kube/issues/122'}};
function fixture() {
  const values = new Map(), tasks = new Map(), receipts = new Set(), creations = new Map();
  const store = {prefix: 'agent:ns:test:', redis: {get: async key => values.get(key) || null, set: async (key, value) => values.set(key, value)},
    get: async id => tasks.get(id),
    create: async (task, key) => { if (creations.has(key)) return tasks.get(creations.get(key)); tasks.set(task.id, task); creations.set(key, task.id); return task; },
    mutate: async (id, key, fn) => { const task = tasks.get(id); if (!receipts.has(id + key)) { fn(task); receipts.add(id + key); } return task; }};
  return {store, tasks};
}
test('GitHub Message-IDs resolve singular issue events, plural issue/comment IDs and reply references', () => {
  const expected = {repository: issue.repository, kind: 'issue', id: '122', url: issue.reference.url};
  assert.deepEqual(githubMessageReference(messageId), expected);
  assert.deepEqual(githubMessageReference('<codemowers/kube/issues/122/9876@github.com>'), expected);
  assert.deepEqual(githubMessageReference('<codemowers/kube/122@github.com>'), expected);
  assert.equal(githubMessageReference('<passmower/passmower/pull/305/review/1@github.com>').kind, 'pr');
  assert.deepEqual(githubNotificationReference({messageId: '<other@localhost>', references: [messageId]}), expected);
  assert.deepEqual(githubMessageReference('<passmower/passmower@github.com>'), {repository: 'passmower/passmower'});
  for (const value of ['<org/repo/issues/1@github.com.evil>', '<org/../issues/1@github.com>', '<org/repo/issue/1@evil.com>'])
    assert.equal(githubMessageReference(value), null);
});

test('issue lookup uses repository-scoped assigned issues only when the ID is missing or not found', async () => {
  const reference = githubMessageReference(messageId), calls = [];
  const lookup = async url => { calls.push(url); return issue; };
  const list = async (repository, login) => { calls.push([repository, login]); return [issue]; };
  assert.deepEqual(await resolveGithubMailIssues(reference, 'agent-smith', lookup, list), [issue]);
  assert.deepEqual(calls, [issue.reference.url]); calls.length = 0;
  await resolveGithubMailIssues(reference, 'agent-smith', async () => { throw {status: 404}; }, list);
  assert.deepEqual(calls, [['codemowers/kube', 'agent-smith']]); calls.length = 0;
  await resolveGithubMailIssues({repository: 'passmower/passmower'}, 'agent-smith', lookup, list);
  assert.deepEqual(calls, [['passmower/passmower', 'agent-smith']]); calls.length = 0;
  await assert.rejects(resolveGithubMailIssues(reference, 'agent-smith', async () => { throw {status: 403}; }, list));
  assert.deepEqual(calls, []);
});

test('email discovery creates one issue task and deduplicates subsequent emails and webhook deliveries', async () => {
  const f = fixture();
  const routed = await routeGithubIssueMail(f.store, 'ns', issue, 'agent-smith', messageId);
  assert.equal(routed.outcome, 'created'); const task = f.tasks.get(routed.taskId);
  assert.equal(task.repository, issue.repository); assert.equal(task.pending.length, 1);
  assert.equal(task.title, 'Implement feature'); assert.equal(task.events[1].content.includes('Private issue specification'), false);
  assert.equal(task.events[0].actor.provider, 'github'); assert.equal(task.events[1].actor.type, 'controller');
  assert.equal((await routeGithubIssueMail(f.store, 'ns', issue, 'agent-smith', messageId)).outcome, 'duplicate');
  assert.equal((await routeGithubIssueMail(f.store, 'ns', issue, 'agent-smith', '<another@github.com>')).outcome, 'duplicate');
  const webhook = githubTask('ns', {}, issue, 'issues', issue.content, 'signed-hook');
  assert.equal((await f.store.create(webhook, 'external:github:codemowers/kube:122')).id, task.id);
  assert.equal(f.tasks.size, 1);
  changeState(task, 'Paused', {type: 'user'});
  const changed = {...issue, updatedAt: '2026-10-12T16:01:00Z'};
  assert.equal((await routeGithubIssueMail(f.store, 'ns', changed, 'agent-smith', '<changed@github.com>')).outcome, 'queued');
  assert.equal(task.desiredState, 'Paused'); assert.equal(task.pending.length, 2);
  await routeGithubIssueMail(f.store, 'ns', {...changed, updatedAt: '2026-10-12T16:02:00Z'}, 'agent-smith', '<older@github.com>', {backfill: true});
  assert.equal(task.pending.length, 2);
});

test('API assignment and issue state decide routing; unrelated email content cannot create work', async () => {
  const f = fixture();
  assert.equal((await routeGithubIssueMail(f.store, 'ns', {...issue, assignees: ['someone']}, 'agent-smith', messageId)).reason, 'not_assigned_to_agent');
  assert.equal((await routeGithubIssueMail(f.store, 'ns', {...issue, state: 'closed'}, 'agent-smith', messageId)).reason, 'closed_without_task');
  assert.equal((await routeGithubIssueMail(f.store, 'ns', {...issue, isPullRequest: true}, 'agent-smith', messageId)).reason, 'pull_request');
  assert.equal(f.tasks.size, 0);
  const routed = await routeGithubIssueMail(f.store, 'ns', issue, 'agent-smith', messageId);
  const result = await routeGithubIssueMail(f.store, 'ns', {...issue, state: 'closed'}, 'agent-smith', '<closed@github.com>');
  assert.equal(result.outcome, 'completed'); assert.equal(f.tasks.get(routed.taskId).phase, 'Completed');
});
