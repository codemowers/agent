import test from 'node:test';
import assert from 'node:assert/strict';
import {newTask, prompt, reserve, finish, changeState, publicTask, Problem, workerEvent, claimNextTurn, renameTask, deleteTask, removeQueuedPrompt, stateRevision, blockExecution, interruptPrompt} from '../src/domain.js';
import {authorized} from '../src/auth.js';
import {Kube} from '../src/kube.js';
test('prompt stop retains the active worker and task, marks interrupted input, and rejects stale turn controls', () => {
  const actor = {type: 'user', name: 'Lauri'};
  const task = newTask('ns', actor, 'First'); const run = reserve(task);
  workerEvent(task, run.id, {kind: 'turn_started', supportsPromptStop: true, startedAt: new Date().toISOString()});
  const ids = [...run.currentPrompts];
  interruptPrompt(task, run.id, ids, actor);
  assert.equal(task.active.id, run.id); assert.equal(task.desiredState, 'Active');
  assert.equal(task.active.stopRequested, undefined);
  workerEvent(task, run.id, {kind: 'turn_result', sequence: 2, result: {exitCode: 130, interrupted: true, summary: 'Prompt stopped'}});
  assert.equal(task.reason, 'PromptStopped'); assert.equal(task.active.state, 'Idle');
  assert.equal(task.events.find(e=>e.id===ids[0]).queueDisposition, 'stopped');
  prompt(task, actor, 'Second', 'webui'); claimNextTurn(task, run.id);
  assert.throws(()=>interruptPrompt(task,run.id,ids,actor), error=>error.status===409);
  assert.equal(task.active.interrupt, undefined);
});
test('stop actions ignore stale UI revisions while resume and completion remain conditional', () => {
  assert.equal(stateRevision('Paused', 1), undefined);
  assert.equal(stateRevision('Cancelled', undefined), undefined);
  assert.equal(stateRevision('Active', 1), 1);
  assert.equal(stateRevision('Completed', 1), 1);
  assert.throws(() => stateRevision('Active', undefined), Problem);
  const task = newTask('ns', {type: 'user'}, 'Build'); reserve(task);
  changeState(task, 'Paused', {type: 'user'});
  assert.equal(task.active.stopRequested, true);
  assert.equal(task.phase, 'Stopping');
  finish(task, task.active.id, {exitCode: 143, summary: 'Stopped'});
  assert.equal(task.phase, 'Paused');
  assert.equal(task.active, null);
});

const user = {type: 'user', issuer: 'https://id.example/', subject: 'user-123', name: 'Lauri'};
test('blockers retain warm workers, preserve the question across idle expiry and accept replies', () => {
  const task = newTask('ns', user, 'Build and deploy', 'webhook'), execution = reserve(task);
  assert.equal(execution.idleTimeoutSeconds, 0);
  blockExecution(task, execution.id, 'Registry destination is unclear. Which provisioned registry should I use?');
  assert.equal(task.desiredState, 'Active'); assert.equal(task.active.stopRequested, undefined);
  assert.equal(task.active.idleTimeoutSeconds, 3600); assert.equal(task.blocked.executionId, execution.id);
  assert.throws(() => blockExecution(task, 'stale', 'Other'), {status: 409});
  const completedAt = new Date(Date.now() - 120000).toISOString();
  workerEvent(task, execution.id, {kind: 'turn_result', sequence: 1, result: {exitCode: 0, summary: 'Awaiting confirmation', completedAt}});
  assert.equal(task.active.state, 'Idle'); assert.equal(task.phase, 'Waiting');
  assert.equal(task.reason, 'AwaitingConfirmation');
  assert.equal(Date.parse(task.active.idleExpiresAt), Date.parse(completedAt) + 3600000);
  const reply = prompt(task, user, 'Use the provisioned registry', 'webui');
  claimNextTurn(task, execution.id);
  assert.equal(task.active.id, execution.id); assert.deepEqual(task.active.currentPrompts, [reply.id]);
  assert.equal(task.blocked, null); assert.equal(task.reason, null);
  blockExecution(task, execution.id, 'Confirm the destination');
  finish(task, execution.id, {exitCode: 0, summary: 'Awaiting confirmation'});
  assert.equal(task.phase, 'Waiting'); assert.equal(task.reason, 'AwaitingConfirmation');
  assert.equal(task.pending.length, 0); assert.equal(task.blocked.summary, 'Confirm the destination');
  prompt(task, user, 'Confirmed');
  reserve(task); assert.equal(task.blocked, null); assert.equal(task.reason, null);
});

test('removing queued input preserves history, skips dispatch, and cannot remove a claimed prompt', () => {
  const task=newTask('ns',user,'First prompt');const run=reserve(task);
  const removed=prompt(task,user,'Discard this followup','webui');
  const kept=prompt(task,user,'Keep this followup','webui');
  removeQueuedPrompt(task,removed.id,user);
  assert.deepEqual(task.pending,[kept.id]);
  assert.equal(task.events.find(e=>e.id===removed.id).queueDisposition,'removed');
  assert.equal(task.events.find(e=>e.id===removed.id).content,'Discard this followup');
  assert.throws(()=>removeQueuedPrompt(task,task.events[0].id,user),{status:409});
  workerEvent(task,run.id,{kind:'turn_result',sequence:1,result:{exitCode:0,summary:'Done'}});
  claimNextTurn(task,run.id);
  assert.deepEqual(task.active.currentPrompts,[kept.id]);
  assert.throws(()=>removeQueuedPrompt(task,kept.id,user),{status:409});
  const waiting=newTask('ns',user,'Never start this');removeQueuedPrompt(waiting,waiting.pending[0],user);
  assert.equal(waiting.phase,'Waiting');assert.equal(reserve(waiting),undefined);
});
test('repository-free tasks keep distinct IDs, creators, and independent sessions', () => {
  const a = newTask('namespace-a', user, 'Implement Etherpad clone');
  const b = newTask('namespace-a', user, 'Implement Etherpad clone');
  assert.notEqual(a.id, b.id);
  assert.equal(a.repository, undefined);
  assert.deepEqual(a.events[0].actor, user);
  const first = reserve(a);
  assert.notEqual(first.sessionId, reserve(b).sessionId);
});
test('events received while running queue for a later execution', () => {
  const task = newTask('ns', user, 'First instruction');
  const first = reserve(task);
  const followup = prompt(task, user, 'Also add tests', 'webui');
  assert.equal(reserve(task), undefined);
  finish(task, first.id, {exitCode: 0, sessionId: first.sessionId, summary: 'Code ready'});
  assert.equal(task.phase, 'Pending');
  const second = reserve(task);
  assert.deepEqual(second.prompts, [followup.id]);
  assert.equal(second.sessionId, first.sessionId);
});
test('pause wins over late results and preserves pending prompts', () => {
  const task = newTask('ns', user, 'Build it'); const first = reserve(task);
  changeState(task, 'Paused', user);
  prompt(task, user, 'Later instruction', 'webui');
  assert.equal(task.phase, 'Stopping');
  finish(task, first.id, {exitCode: 0, summary: 'Done'});
  assert.equal(task.phase, 'Paused'); assert.equal(task.pending.length, 2);
  assert.equal(reserve(task), undefined);
  changeState(task, 'Active', user); assert.ok(reserve(task));
});
test('stale workers cannot change another execution and public state hides tokens', () => {
  const task = newTask('ns', user, 'Build it'); const run = reserve(task);
  assert.throws(() => finish(task, 'old-attempt', {}), Problem);
  assert.equal(publicTask(task).active.token, undefined);
  assert.ok(task.active.token);
  finish(task, run.id, {exitCode: 0, summary: 'Completed'});
  assert.equal(task.phase, 'Waiting'); // CLI claim is not business completion.
  changeState(task, 'Completed', user); assert.equal(task.phase, 'Completed');
});
test('namespace authorization requires scope and verified membership', () => {
  const config = {namespace: 'customer-a', allowedGroups: [], allowedSubjects: []};
  const claims = {iss: user.issuer, sub: user.subject, scope: 'agent:read', 'codemowers.io/namespaces': ['customer-a']};
  assert.equal(authorized(claims, config, 'agent:read').subject, user.subject);
  assert.throws(() => authorized({...claims, 'codemowers.io/namespaces': ['customer-b']}, config, 'agent:read'), Problem);
  assert.throws(() => authorized(claims, config, 'agent:write'), Problem);
});
test('worker uses user namespaces, separate ServiceAccount, Task PVC, and excludes OIDC/Redis credentials', () => {
  const config = {namespace: 'tenant', release: 'agent', image: 'codemowers/agent:sha',
    anthropicSecret: 'model', githubSecret: 'git', internalURL: 'https://agent.example', model: 'configured', budget: 5};
  const task = newTask('tenant-uid', user, 'Build it'); reserve(task);
  const kube = new Kube(config), pod = kube.pod(task);
  assert.equal(pod.spec.runtimeClassName, undefined);
  assert.equal(pod.spec.hostUsers, false);
  assert.equal(pod.spec.restartPolicy, 'Never');
  assert.equal(pod.spec.serviceAccountName, 'agent-worker');
  assert.equal(pod.spec.volumes[0].persistentVolumeClaim.claimName, `agent-task-${task.id}`);
  assert.ok(pod.metadata.finalizers.includes('agents.codemowers.io/result-recorded'));
  const env = pod.spec.containers[0].env.map(e => e.name);
  assert.ok(!env.some(name => name.startsWith('OIDC') || name.startsWith('REDIS')));
  assert.equal(JSON.parse(kube.envelope(task).data['envelope.json']).sessionId, task.active.sessionId);
  assert.equal(pod.spec.containers[0].env.find(e => e.name === 'AGENT_IDLE_TIMEOUT_SECONDS').value, '3600');
});
test('resident turns consume queued prompts once, avoid duplicate results, and pause requeues only unfinished work', () => {
  const task = newTask('ns', user, 'First'); const run = reserve(task);
  workerEvent(task, run.id, {kind: 'progress', summary: 'Hello'});
  const completedAt = new Date(Date.now() - 120000).toISOString();
  workerEvent(task, run.id, {kind: 'turn_result', sequence: 2, result: {exitCode: 0, summary: 'Hello', completedAt, metrics: {durationMs: 1000, outputTokens: 12}}});
  assert.equal(task.events.filter(e => e.content === 'Hello').length, 1);
  assert.deepEqual(task.events.find(e=>e.content==='Hello').metrics,{durationMs:1000,outputTokens:12});
  assert.equal(task.active.state, 'Idle'); assert.equal(task.phase, 'Waiting');
  assert.equal(Date.parse(task.active.idleExpiresAt), Date.parse(completedAt) + 3600000);
  const followup = prompt(task, user, 'Second', 'webui');
  claimNextTurn(task, run.id); claimNextTurn(task, run.id);
  assert.deepEqual(task.active.currentPrompts, [followup.id]);
  assert.equal(task.pending.length, 0); assert.equal(task.active.id, run.id);
  changeState(task, 'Paused', user);
  finish(task, run.id, {exitCode: 143, summary: 'Interrupted'});
  assert.deepEqual(task.pending, [followup.id]);
  const eventTask = newTask('ns', user, 'Webhook task', 'webhook');
  assert.equal(reserve(eventTask).idleTimeoutSeconds, 0);
});
test('renaming preserves task identity and objective; deletion blocks new work and stops active execution', () => {
  const task = newTask('ns', user, 'Build thing'); const taskId = task.id;
  renameTask(task, 'New name', user);
  assert.equal(task.title, 'New name'); assert.equal(task.objective, 'Build thing'); assert.equal(task.id, taskId);
  reserve(task); deleteTask(task, user);
  assert.equal(task.phase, 'Stopping'); assert.equal(task.active.stopRequested, true);
  assert.throws(() => prompt(task, user, 'More', 'webui'), Problem);
  assert.throws(() => renameTask(task, 'Again', user), Problem);
});
