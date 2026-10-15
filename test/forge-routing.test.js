import test from 'node:test';
import assert from 'node:assert/strict';
import {createHmac} from 'node:crypto';
import {forgeWebhook, forgeConfiguration, forgeReference, threadKey, findThreadTask} from '../src/forge.js';
import {githubTask, githubEventURL, routeGithubIssueEvent, routeGithubPullRequest, reconcileGithubAssignments, reportedPullRequests} from '../src/github.js';
import {routeGithubIssueMail, githubMessageReference} from '../src/github-mail.js';
import {taskCommunication} from '../src/agent-mcp.js';
import {reserve, changeState, digest, prompt, controllerActor, workerEvent, claimNextTurn} from '../src/domain.js';
import {SetupCatalog} from '../src/setup.js';

const login = 'agent-smith', actor = {type: 'external', provider: 'github', name: 'sam'};
function fixture() {
  const tasks = new Map(), values = new Map(), receipts = new Set(), creations = new Map();
  const store = {prefix: 'test:', redis: {get: async key => values.get(key), set: async (key, value) => values.set(key, value)},
    list: async () => [...tasks.values()], get: async id => {
      if (!tasks.has(id)) throw Object.assign(new Error('Missing task'), {status: 404});
      return tasks.get(id);
    }, create: async (task, key) => {
      if (creations.has(key)) return tasks.get(creations.get(key));
      tasks.set(task.id, task); creations.set(key, task.id); return task;
    }, mutate: async (id, key, fn) => {
      const task = tasks.get(id), receipt = id + ':' + key;
      if (!receipts.has(receipt)) { fn(task); receipts.add(receipt); }
      return task;
    }};
  return {store, tasks, values};
}
const item = (kind = 'issue', forge = forgeConfiguration({})) => ({
  reference: forgeReference(`${forge.serverURL}/org/repo/${kind === 'issue' ? 'issues' : forge.provider === 'gitea' ? 'pulls' : 'pull'}/42`, forge),
  repository: 'org/repo', state: 'open', assignees: [login], author: 'sam', title: 'Implement feature', content: 'Specification',
});
const event = (deliveryId, action = 'created') => ({actor, eventType: 'issue_comment', action, deliveryId, comment: 'Feedback'});

test('Git source events link to verified comment anchors without repeating the original body on updates', async () => {
  const {store, tasks} = fixture(), issue = item();
  const anchor = issue.reference.url + '#issuecomment-123';
  assert.equal(githubEventURL(issue.reference, anchor), anchor);
  assert.equal(githubEventURL(issue.reference, 'https://evil.example/issues/42#issuecomment-123'), issue.reference.url);
  assert.equal(githubEventURL(issue.reference, 'https://github.com/org/repo/issues/43#issuecomment-123'), issue.reference.url);
  const created = await routeGithubIssueEvent(store, 'ns', issue, login, {...event('initial'), commentURL: anchor});
  const task = tasks.get(created.taskId);
  assert.equal(task.events[0].externalURL, anchor);
  await routeGithubIssueEvent(store, 'ns', issue, login, {actor, eventType: 'issues', action: 'edited', deliveryId: 'update'});
  const update = task.events.find(entry => entry.deliveryId === 'update');
  assert.equal(update.content.includes(issue.content), false);
  assert.equal(update.externalURL, issue.reference.url);
});

test('routing keys separate issues, PRs, providers and servers; URLs stay on the configured server', () => {
  const enterprise = forgeConfiguration({AGENT_FORGE_URL: 'https://git.example.test'});
  const gitea = forgeConfiguration({AGENT_FORGE_PROVIDER: 'gitea', AGENT_FORGE_URL: 'https://code.example.test/team'});
  assert.equal(enterprise.apiURL, 'https://git.example.test/api/v3');
  assert.equal(gitea.apiURL, 'https://code.example.test/team/api/v1');
  assert.equal(new Set([item().reference, item('pr').reference, item('issue', enterprise).reference,
    item('issue', gitea).reference].map(threadKey)).size, 4);
  assert.equal(forgeReference('https://github.com/org/repo/issues/42', enterprise), null);
  assert.equal(forgeReference('https://token@git.example.test/org/repo/pull/42', enterprise), null);
  assert.equal(forgeReference('https://git.example.test/org/repo/pull/42?token=secret', enterprise), null);
  assert.equal(forgeReference('https://code.example.test/team/org/repo/pull/42', gitea), null);
  assert.equal(item('pr', gitea).reference.kind, 'pr');
  assert.equal(githubMessageReference('<org/repo/issue/42/event/1@git.example.test>', enterprise).url, item('issue', enterprise).reference.url);
  assert.equal(githubMessageReference('<org/repo/issue/42/event/1@github.com>', enterprise), null);
});

test('existing issue tasks retain unassigned source events while rapid follow-ups stay silent', async t => {
  t.mock.timers.enable({apis: ['Date'], now: Date.now()});
  const {store, tasks, values} = fixture(), issue = item();
  assert.equal((await routeGithubIssueEvent(store, 'ns', {...issue, assignees: []}, login, event('unrelated'))).reason, 'not_assigned_to_agent');
  const created = await routeGithubIssueEvent(store, 'ns', issue, login, event('first'));
  const task = tasks.get(created.taskId); task.pending = [];
  // A restarted controller can restore its routing index from durable references.
  values.clear();
  const unassigned = {...issue, assignees: []};
  assert.equal((await routeGithubIssueEvent(store, 'ns', unassigned, login, event('edited', 'edited'))).outcome, 'resumed');
  assert.equal((await routeGithubIssueMail(store, 'ns', unassigned, login, 'notification-1')).outcome, 'resumed');
  assert.equal((await routeGithubIssueEvent(store, 'ns', unassigned, login, event('deleted', 'deleted'))).outcome, 'resumed');
  assert.equal(tasks.size, 1); assert.equal(task.pending.length, 0);
  assert.equal(task.events.filter(entry => entry.kind === 'event').length, 4);
  assert.equal((await routeGithubIssueEvent(store, 'ns', unassigned, login, event('deleted', 'deleted'))).outcome, 'duplicate');
  assert.equal(task.pending.length, 0);
  assert.equal((await routeGithubIssueEvent(store, 'ns', unassigned, login, {...event('self'), actor: {...actor, name: login}})).reason, 'agent_authored_event');
  assert.equal(task.pending.length, 0);
  changeState(task, 'Paused', actor);
  t.mock.timers.tick(180000);
  assert.equal((await routeGithubIssueEvent(store, 'ns', unassigned, login, event('paused'))).outcome, 'queued');
  assert.equal(task.desiredState, 'Paused'); assert.equal(task.pending.length, 1);
  await routeGithubIssueEvent(store, 'ns', {...unassigned, state: 'closed'}, login, event('closed'));
  assert.equal(task.phase, 'Completed'); assert.equal(task.deleteRequested, undefined);
});

test('assigned PR recovery creates a distinct warm task and keeps tracking it after unassignment', async t => {
  t.mock.timers.enable({apis: ['Date'], now: Date.now()});
  const {store, tasks} = fixture(), pull = item('pr');
  await routeGithubIssueEvent(store, 'ns', item(), login, event('issue'));
  assert.equal((await routeGithubPullRequest(store, 'ns', {...pull, assignees: []}, login, {actor, deliveryId: 'unrelated'})).reason, 'not_assigned_to_agent');
  const created = await routeGithubPullRequest(store, 'ns', pull, login, {actor, deliveryId: 'assignment', assignment: true});
  assert.equal(created.outcome, 'created'); assert.equal(tasks.size, 2);
  const task = tasks.get(created.taskIds[0]);
  assert.equal(taskCommunication(task).primary.channel, 'github_pull_request');
  t.mock.timers.tick(300000);
  const execution = reserve(task); assert.equal(execution.idleTimeoutSeconds, 3600);
  const options = {actor, commentAction: 'deleted', deliveryId: 'deleted-comment', comment: 'Deleted feedback'};
  assert.equal((await routeGithubPullRequest(store, 'ns', {...pull, assignees: []}, login, options)).outcome, 'resumed');
  assert.equal(task.pending.length, 1);
  assert.equal((await routeGithubPullRequest(store, 'ns', {...pull, assignees: []}, login, options)).outcome, 'duplicate');
  assert.equal(task.pending.length, 1);
  assert.equal((await routeGithubPullRequest(store, 'ns', pull, login, {...options, actor: {...actor, name: login}})).reason, 'agent_authored_event');
  changeState(task, 'Cancelled', actor);
  assert.equal((await routeGithubPullRequest(store, 'ns', pull, login, {...options, deliveryId: 'late'})).outcome, 'ignored');
});

test('linked PR updates stay on their issue task even after unassignment', async t => {
  t.mock.timers.enable({apis: ['Date'], now: Date.now()});
  const {store, tasks} = fixture(), issue = item(), pull = item('pr');
  const task = await store.create(githubTask('ns', actor, issue, 'issues', issue.content, 'issue'), 'issue');
  task.references.push(pull.reference); task.pending = []; task.desiredState = 'Paused'; task.phase = 'Paused';
  t.mock.timers.tick(180000);
  const routed = await routeGithubPullRequest(store, 'ns', {...pull, assignees: []}, login, {actor, deliveryId: 'unassigned'});
  assert.deepEqual(routed.taskIds, [task.id]); assert.equal(tasks.size, 1);
  assert.equal(taskCommunication(task).primary.url, issue.reference.url);
  assert.equal(task.pending.length, 1); assert.equal(task.desiredState, 'Paused');
});

test('startup assignment recovery creates only missing, API-verified open assignments without read cursors', async () => {
  const {store, tasks} = fixture(), issue = item(), pull = item('pr');
  const unassigned = {...item(), reference: {...issue.reference, id: '43', url: issue.reference.url.replace('/42', '/43')}, assignees: []};
  const lookup = async reference => reference.kind === 'pr' ? pull : reference.id === '43' ? unassigned : issue;
  const refs = [issue.reference, pull.reference, unassigned.reference];
  assert.equal((await reconcileGithubAssignments(store, 'ns', refs, lookup, login)).created, 2);
  assert.equal(tasks.size, 2);
  assert.equal((await reconcileGithubAssignments(store, 'ns', refs, lookup, login)).created, 0);
  assert.ok([...tasks.values()].every(task => task.pending.length === 1 && !Object.hasOwn(task, 'githubIssueUpdatedAt')));
  const task = [...tasks.values()][0];
  const stale = store.prefix + 'external:' + digest(threadKey(issue.reference));
  await store.redis.set(stale, 'deleted-task');
  assert.equal((await findThreadTask(store, issue.reference)).id, task.id);
});

test('GHE and Gitea assignment search use their own API and share the same routing semantics', async () => {
  for (const provider of ['github', 'gitea']) {
    const forge = forgeConfiguration({AGENT_FORGE_PROVIDER: provider, AGENT_FORGE_URL: 'https://git.example.test'});
    const issue = item('issue', forge), pull = item('pr', forge), urls = [];
    const setup = new SetupCatalog({forge}, async url => {
      urls.push(url);
      const entries = [{html_url: issue.reference.url}, {html_url: pull.reference.url}, {html_url: 'https://evil.test/org/repo/issues/1'}];
      return new Response(JSON.stringify(provider === 'github' ? {items: entries, total_count: 3} : entries));
    });
    const catalog = await setup.assignedReferences(login);
    assert.equal(catalog.references.length, 2);
    assert.ok(urls[0].startsWith(forge.apiURL + (provider === 'github' ? '/search/issues?' : '/repos/issues/search?')));
    assert.equal(new URL(urls[0]).searchParams.get(provider === 'github' ? 'q' : 'assigned'), provider === 'github' ? 'is:open assignee:' + login : 'true');
    const {store, tasks} = fixture();
    assert.equal((await reconcileGithubAssignments(store, 'ns', catalog.references, async reference => reference.kind === 'pr' ? pull : issue, login)).created, 2);
    assert.equal(tasks.size, 2);
    assert.equal(taskCommunication([...tasks.values()][1]).primary.channel, provider + '_pull_request');
    const task = [...tasks.values()][0];
    task.events.push({actor: {type: 'agent'}, kind: 'progress', content: pull.reference.url});
    assert.equal(reportedPullRequests(task, forge)[0].url, pull.reference.url);
  }
});


test('GitHub/GHE and Gitea authenticate the exact webhook bytes using their own headers', () => {
  const bytes = Buffer.from('{"action":"assigned"}'), secret = 'fixture-signing-secret';
  const signature = createHmac('sha256', secret).update(bytes).digest('hex');
  for (const provider of ['github', 'gitea']) {
    const config = {provider}, prefix = provider === 'github' ? 'x-github-' : 'x-gitea-';
    const headers = {[prefix + 'event']: 'issue_comment', [prefix + 'delivery']: 'delivery-1',
      [provider === 'github' ? 'x-hub-signature-256' : 'x-gitea-signature']: provider === 'github' ? 'sha256=' + signature : signature};
    assert.deepEqual(forgeWebhook(headers, bytes, config, secret), {authenticated: true, eventType: 'issue_comment', deliveryId: 'delivery-1'});
    assert.equal(forgeWebhook(headers, Buffer.from('{}'), config, secret).authenticated, false);
    assert.equal(forgeWebhook(headers, bytes, config, undefined).authenticated, false);
    assert.equal(forgeWebhook(headers, bytes, {provider: provider === 'github' ? 'gitea' : 'github'}, secret).authenticated, false);
  }
  assert.equal(threadKey(item().reference), threadKey({...item().reference, repository: 'ORG/Repo'}));
});


test('webhook and notification mail share one immediate wakeup in either arrival order', async t => {
  t.mock.timers.enable({apis: ['Date'], now: Date.now()});
  for (const mailFirst of [false, true]) {
    const {store, tasks} = fixture(), issue = item();
    const hook = () => routeGithubIssueEvent(store, 'ns', issue, login, event('hook'));
    const mail = () => routeGithubIssueMail(store, 'ns', issue, login, 'notification');
    await (mailFirst ? mail() : hook());
    const task = [...tasks.values()][0], id = task.pending[0], deadline = task.notificationSuppressUntil;
    assert.equal(Date.parse(deadline) - Date.now(), 180000);
    t.mock.timers.tick(30000);
    await (mailFirst ? hook() : mail());
    assert.equal(task.notificationSuppressUntil, deadline);
    assert.deepEqual(task.pending, [id]);
    const queued = task.events.find(entry => entry.id === id);
    assert.equal(queued.notificationKey, threadKey(issue.reference));
    assert.equal(queued.causedBy.length, 2);
    assert.ok(queued.causedBy.every(cause => task.events.some(entry => entry.id === cause && entry.kind === 'event')));
    const count = task.events.length;
    await hook(); await mail();
    assert.equal(task.events.length, count); assert.equal(queued.causedBy.length, 2);
    const run = reserve(task);
    assert.deepEqual(run.currentPrompts, [id]);
    await routeGithubIssueMail(store, 'ns', issue, login, 'new-notification');
    assert.deepEqual(task.pending, []);
    t.mock.timers.tick(150000);
    await routeGithubIssueMail(store, 'ns', issue, login, 'later-notification');
    assert.equal(task.pending.length, 1); assert.notEqual(task.pending[0], id);
  }
});

test('notifications during a running turn are suppressed while human prompts stay separate', async t => {
  t.mock.timers.enable({apis: ['Date'], now: Date.now()});
  const {store, tasks} = fixture(), issue = item();
  await routeGithubIssueEvent(store, 'ns', issue, login, event('initial'));
  const task = [...tasks.values()][0];
  const run = reserve(task);
  const running = structuredClone(task.events.find(entry => entry.id === run.currentPrompts[0]));
  await routeGithubIssueEvent(store, 'ns', issue, login, event('followup-hook'));
  assert.deepEqual(task.pending, []);
  const human = prompt(task, {type: 'user', name: 'sam'}, 'Please also document it.', 'webui');
  await routeGithubIssueMail(store, 'ns', issue, login, 'followup-mail');
  assert.deepEqual(task.pending, [human.id]);
  assert.deepEqual(task.events.find(entry => entry.id === running.id), running);
  workerEvent(task, run.id, {kind: 'turn_result', sequence: 1, result: {exitCode: 0, summary: 'Done'}});
  claimNextTurn(task, run.id);
  assert.deepEqual(task.active.currentPrompts, [human.id]);
  t.mock.timers.tick(180000);
  await routeGithubIssueMail(store, 'ns', issue, login, 'later-mail');
  assert.equal(task.pending.length, 1);
});

test('PR mail and webhook wakeups fold after task suppression expires', async t => {
  t.mock.timers.enable({apis: ['Date'], now: Date.now()});
  const {store, tasks} = fixture(), pull = item('pr'), issue = item();
  const task = await store.create(githubTask('ns', actor, issue, 'issues', issue.content, 'issue'), 'issue');
  task.references.push(pull.reference); task.desiredState = 'Paused'; task.phase = 'Paused';
  t.mock.timers.tick(180000);
  await routeGithubPullRequest(store, 'ns', pull, login, {actor, deliveryId: 'pr-hook', commentAction: 'created', comment: 'Review'});
  const pr = task.pending[1];
  await routeGithubPullRequest(store, 'ns', pull, login, {actor: {...controllerActor}, deliveryId: 'pr-mail'});
  assert.equal(tasks.size, 1); assert.equal(task.pending.length, 2);
  assert.equal(task.pending[1], pr); assert.equal(task.desiredState, 'Paused');
  assert.equal(task.events.find(entry => entry.id === pr).causedBy.length, 2);
  assert.notEqual(task.events.find(entry => entry.id === task.pending[0]).notificationKey,
    task.events.find(entry => entry.id === pr).notificationKey);
});
