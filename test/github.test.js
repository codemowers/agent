import test from 'node:test';
import assert from 'node:assert/strict';
import {githubTask, githubWakeup, closeGithubTask, reopenGithubTask, reconcileGithubIssues, reportedPullRequests, reconcileGithubPullRequests, restoreGithubIssue} from '../src/github.js';
import {reserve, finish, prompt, workerEvent, claimNextTurn, controllerActor, activity, changeState} from '../src/domain.js';
import {Kube} from '../src/kube.js';

test('issue comments reuse a warm worker and reset idle time from the last reply', t => {
  t.mock.timers.enable({apis: ['Date'], now: Date.now()});
  const issue = {content: 'Implement feature', repository: 'codemowers/pitchdeck',
    reference: {source: 'github', repository: 'codemowers/pitchdeck', kind: 'issue', id: '1'}};
  const task = githubTask('llm', {type: 'external', name: 'alexriver'}, issue, 'issues', issue.content, 'assignment');
  assert.equal(task.origin, 'webhook');
  t.mock.timers.tick(300000);
  const run = reserve(task), podName = run.podName;
  assert.equal(run.idleTimeoutSeconds, 3600);
  const kube = new Kube({release: 'agent', namespace: 'llm', image: 'runner', internalURL: 'http://controller'});
  assert.equal(kube.pod(task).spec.containers[0].env.find(e => e.name === 'AGENT_IDLE_TIMEOUT_SECONDS').value, '3600');
  const first = new Date(Date.now() - 120000).toISOString();
  workerEvent(task, run.id, {kind: 'turn_result', sequence: 1, result: {exitCode: 0, summary: 'First reply', completedAt: first}});
  const comment = prompt(task, controllerActor, githubWakeup('issue_comment', issue.reference.url), 'controller');
  claimNextTurn(task, run.id);
  assert.equal(task.active.podName, podName);
  assert.deepEqual(task.active.currentPrompts, [comment.id]);
  const second = new Date().toISOString();
  workerEvent(task, run.id, {kind: 'turn_result', sequence: 2, result: {exitCode: 0, summary: 'Updated feature', completedAt: second}});
  assert.equal(task.active.state, 'Idle');
  assert.equal(Date.parse(task.active.idleExpiresAt), Date.parse(second) + 3600000);
  finish(task, run.id, {exitCode: 0, summary: 'Updated feature'});
  prompt(task, controllerActor, 'Another issue comment after idle shutdown', 'controller');
  assert.equal(reserve(task).idleTimeoutSeconds, 3600);
});

test('GitHub closure is business completion, survives late worker exit, and blocks new prompts', t => {
  t.mock.timers.enable({apis: ['Date'], now: Date.now()});
  const issue={content:'Task',repository:'codemowers/pitchdeck',reference:{source:'github',kind:'issue',id:'1',repository:'codemowers/pitchdeck',url:'https://github.com/codemowers/pitchdeck/issues/1'}};
  const actor={type:'external',name:'GitHub'};
  const task=githubTask('llm',actor,issue,'issues',issue.content,'first');
  t.mock.timers.tick(300000);
  const execution=reserve(task);
  closeGithubTask(task,actor,issue.reference.url);
  assert.equal(task.phase,'Stopping');assert.equal(task.active.stopRequested,true);
  assert.throws(()=>prompt(task,actor,'Extra work','webhook'),{status:409});
  finish(task,execution.id,{exitCode:143,summary:'Stopped after issue closure'});
  assert.equal(task.phase,'Completed');assert.equal(task.reason,'GitHubIssueClosed');
  assert.equal(task.pending.length,0);
  const events=task.events.length;closeGithubTask(task,actor,issue.reference.url);
  assert.equal(task.events.length,events);
});

test('startup reconciliation discovers missed GitHub closure without a manual webhook replay', async () => {
  const issue={content:'Already merged',repository:'codemowers/pitchdeck',reference:{source:'github',kind:'issue',id:'1',repository:'codemowers/pitchdeck',url:'https://github.com/codemowers/pitchdeck/issues/1'}};
  const task=githubTask('llm',{type:'external'},issue,'issues',issue.content,'first');
  const failed={...structuredClone(task),id:'unavailable',references:[{...issue.reference,url:'https://github.com/codemowers/unavailable/issues/2'}]};
  const store={list:async()=>[failed,task],mutate:async(id,key,fn)=>{assert.equal(id,task.id);fn(task);}};
  await reconcileGithubIssues(store,async url=>{if(url.includes('unavailable'))throw Error('Unavailable');return {state:'closed',closedAt:'2026-10-11T21:00:00Z'};});
  assert.equal(task.phase,'Completed');assert.equal(task.active,null);
  assert.equal(task.reason,'GitHubIssueClosed');
});

test('reopenGithubTask resets closure state and adds a control event; idempotent and blocked by cancel', () => {
  const issue={content:'Task',repository:'codemowers/pitchdeck',reference:{source:'github',kind:'issue',id:'1',repository:'codemowers/pitchdeck',url:'https://github.com/codemowers/pitchdeck/issues/1'}};
  const actor={type:'external',name:'GitHub'};
  const task=githubTask('llm',actor,issue,'issues',issue.content,'first');
  closeGithubTask(task,actor,issue.reference.url);
  assert.equal(task.phase,'Completed');assert.equal(task.completionRequested,true);
  const events=task.events.length;
  const result=reopenGithubTask(task,actor,issue.reference.url);
  assert.equal(result,true);
  assert.equal(task.phase,'Pending');assert.equal(task.completionRequested,false);
  assert.equal(task.githubIssueState,'open');assert.equal(task.desiredState,'Active');
  assert.equal(task.events.length,events+1);
  assert.equal(task.events.at(-1).kind,'control');
  // idempotent when already open
  const events2=task.events.length;
  assert.equal(reopenGithubTask(task,actor,issue.reference.url),false);
  assert.equal(task.events.length,events2);
  // blocked when cancelled
  const task2=githubTask('llm',actor,issue,'issues',issue.content,'second');
  closeGithubTask(task2,actor,issue.reference.url); task2.desiredState='Cancelled';
  assert.equal(reopenGithubTask(task2,actor,issue.reference.url),false);
});

test('reconciliation reopens a task when GitHub issue is reopened after a missed webhook', async () => {
  const issue={content:'Rolled back',repository:'codemowers/pitchdeck',reference:{source:'github',kind:'issue',id:'1',repository:'codemowers/pitchdeck',url:'https://github.com/codemowers/pitchdeck/issues/1'}};
  const task=githubTask('llm',{type:'external'},issue,'issues',issue.content,'first');
  // Simulate initial execution consumed the prompt before the issue was closed by a merged PR.
  task.pending=[];
  closeGithubTask(task,controllerActor,issue.reference.url);
  assert.equal(task.phase,'Completed');assert.equal(task.pending.length,0);
  const store={list:async()=>[task],mutate:async(id,key,fn)=>{fn(task);}};
  await reconcileGithubIssues(store,async()=>({state:'open',updatedAt:'2026-10-14T10:00:00Z'}));
  assert.equal(task.phase,'Pending');assert.equal(task.completionRequested,false);
  assert.equal(task.desiredState,'Active');assert.equal(task.pending.length,1);
  assert.match(task.events.at(-2).content,/reopened/i);
  // second reconcile cycle is idempotent because store receipt blocks re-mutation; simulate
  // by checking the task is now closeable (open phase) so lookup fires exactly once
  let lookupCalls=0;
  const store2={list:async()=>[task],mutate:async(_id,_key,_fn)=>{}};
  await reconcileGithubIssues(store2,async()=>{lookupCalls++;return {state:'open',updatedAt:'2026-10-14T10:00:00Z'};});
  assert.equal(lookupCalls,1);
});

test('a missing repository retires issue and related PR polling without claiming completion', async () => {
  const issue = {content: 'Delete the repository', repository: 'example/deleted', reference: {
    source: 'github', kind: 'issue', repository: 'example/deleted', id: '1', url: 'https://github.com/example/deleted/issues/1'}};
  const task = githubTask('tenant', {type: 'external'}, issue, 'issues', issue.content, 'created');
  task.references.push({source: 'github', kind: 'pr', repository: issue.repository, id: '2', url: 'https://github.com/example/deleted/pull/2'});
  task.pending = [];
  activity(task, {type: 'agent'}, 'https://github.com/example/deleted/pull/3', 'controller', {kind: 'progress'});
  let issueCalls = 0, prCalls = 0;
  const store = {list: async () => [task], mutate: async (id, receipt, fn) => { assert.equal(id, task.id); fn(task); }};
  const missing = async () => { issueCalls++; throw Object.assign(new Error('private upstream body'), {status: 404, repositoryUnavailable: true}); };
  await reconcileGithubIssues(store, missing);
  await reconcileGithubIssues(store, missing);
  await reconcileGithubPullRequests(store, async () => { prCalls++; }, 'agent-smith');
  assert.equal(issueCalls, 1); assert.equal(prCalls, 0);
  assert.equal(task.reason, 'GitHubRepositoryUnavailable'); assert.equal(task.githubIssueState, 'unavailable');
  assert.equal(task.completionRequested, undefined); assert.notEqual(task.phase, 'Completed');
  assert.equal(task.events.filter(event => event.kind === 'recovery').length, 1);
  assert.ok(!JSON.stringify(task).includes('private upstream body'));
  changeState(task, 'Active', {type: 'user'});
  assert.ok(task.references.every(reference => !reference.unavailable));
  await reconcileGithubIssues(store, missing); assert.equal(issueCalls, 2);
  restoreGithubIssue(task, {...issue, state: 'open'});
  assert.equal(task.githubIssueState, 'open'); assert.equal(task.reason, null);
  assert.ok(task.references.every(reference => !reference.unavailable));
});

test('an unavailable issue does not retire PRs in an accessible repository', async () => {
  const task = githubTask('tenant', {}, {content: 'Work', repository: 'example/available', reference: {
    source: 'github', kind: 'issue', repository: 'example/available', id: '1', url: 'https://github.com/example/available/issues/1'}}, 'issues', 'Work', 'created');
  task.references.push({source: 'github', kind: 'pr', repository: task.repository, id: '2', url: 'https://github.com/example/available/pull/2'});
  const store = {list: async () => [task], mutate: async (id, receipt, fn) => fn(task)};
  await reconcileGithubIssues(store, async () => { throw Object.assign(new Error(), {status: 410}); });
  assert.equal(task.reason, 'GitHubIssueUnavailable');
  assert.equal(task.references[0].unavailable.status, 410); assert.equal(task.references[1].unavailable, undefined);
});

test('an initial GitHub delivery queues one controller prompt with attributed source context', () => {
  const issue = {content: 'Yeet video-1min.md', repository: 'codemowers/pitchdeck',
    reference: {source: 'github', repository: 'codemowers/pitchdeck', kind: 'issue', id: '1'}};
  const actor = {type: 'external', name: 'alexriver', provider: 'github'};
  const task = githubTask('llm', actor, issue, 'issues', issue.content, 'delivery-1');
  assert.equal(task.repository, issue.repository);
  assert.equal(task.pending.length, 1);
  assert.equal(task.events[0].kind, 'event');
  assert.equal(task.events[0].deliveryId, 'delivery-1');
  assert.deepEqual(task.events[0].actor, actor);
  assert.equal(task.events[1].actor.type, 'controller');
  assert.deepEqual(task.events[1].causedBy, [task.events[0].id]);
  assert.deepEqual(task.pending, [task.events[1].id]);
});

test('cross-repository merged PR wakes its issue task once on startup, without replaying older merges', async t => {
  t.mock.timers.enable({apis: ['Date'], now: Date.now()});
  const issue = {content: 'Project objective', repository: 'codemowers/kube',
    reference: {source: 'github', repository: 'codemowers/kube', kind: 'issue', id: '121'}};
  const task = githubTask('llm', {type: 'external'}, issue, 'issues', issue.content, 'first');
  task.pending = [];
  t.mock.timers.tick(180000);
  task.lastExecution = {finishedAt: '2026-10-12T12:14:00Z', result: {completedAt: '2026-10-12T12:14:00Z'}};
  task.events.push({actor: {type: 'agent'}, kind: 'result', createdAt: '2026-10-12T12:13:00Z',
    content: 'Already merged https://github.com/passmower/passmower/pull/304; opened https://github.com/passmower/passmower/pull/305'});
  const store = {list: async () => [task], mutate: async (id, receipt, fn) => { assert.equal(id, task.id); fn(task); return task; }};
  const lookup = async url => ({title: 'Related PR', author: 'agent-smith', state: 'closed', merged: true,
    mergedAt: url.endsWith('/304') ? '2026-10-12T12:00:00Z' : '2026-10-12T12:20:00Z'});
  assert.equal(reportedPullRequests(task).length, 2);
  assert.deepEqual(await reconcileGithubPullRequests(store, lookup, 'agent-smith'), [task.id]);
  assert.equal(task.repository, 'codemowers/kube'); assert.equal(task.pending.length, 1);
  assert.match(task.events.at(-1).content, /passmower\/passmower\/pull\/305/);
  assert.deepEqual(await reconcileGithubPullRequests(store, lookup, 'agent-smith'), []);
  assert.equal(task.pending.length, 1);
});

test('PR notifications preserve pauses and ignore unowned transcript links and completed tasks', async t => {
  t.mock.timers.enable({apis: ['Date'], now: Date.now()});
  const issue = {content: 'Project objective', repository: 'org/source',
    reference: {source: 'github', repository: 'org/source', kind: 'issue', id: '1'}};
  const task = githubTask('llm', {}, issue, 'issues', issue.content, 'first'); task.pending = []; task.desiredState = 'Paused'; task.phase = 'Paused';
  t.mock.timers.tick(180000);
  task.events.push({actor: {type: 'agent'}, kind: 'progress', createdAt: '2026-10-12T12:00:00Z',
    content: 'Created https://github.com/elsewhere/output/pull/2'});
  task.createdAt = '2026-10-12T11:00:00Z';
  const store = {list: async () => [task], mutate: async (id, receipt, fn) => { fn(task); return task; }};
  const merged = author => ({title: 'Output', author, state: 'closed', merged: true, mergedAt: '2026-10-12T12:20:00Z'});
  await reconcileGithubPullRequests(store, async () => merged('someone-else'), 'agent-smith');
  assert.equal(task.pending.length, 0);
  await reconcileGithubPullRequests(store, async () => merged('agent-smith'), 'agent-smith');
  assert.equal(task.pending.length, 1); assert.equal(task.desiredState, 'Paused');
  task.phase = 'Completed';
  let queried = false; await reconcileGithubPullRequests(store, async () => { queried = true; }, 'agent-smith');
  assert.equal(queried, false);
});
