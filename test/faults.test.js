import test from 'node:test';
import assert from 'node:assert/strict';
import {EventEmitter} from 'node:events';
import {setTimeout as delay} from 'node:timers/promises';
import {installFaultHandler, createRepairTask, faultDetails, faultFingerprint} from '../src/faults.js';
import {digest} from '../src/domain.js';

function fixture() {
  const values = new Map(), tasks = new Map(), mutations = new Set();
  const store = {prefix: 'agent:test:', redis: {
    async get(key) { return values.get(key) || null; },
    async set(key, value, options = {}) { if (options.NX && values.has(key)) return null; values.set(key, value); return 'OK'; }},
    async get(id) { if (!tasks.has(id)) throw Object.assign(new Error('Not found'), {status: 404}); return tasks.get(id); },
    async list() { return [...tasks.values()]; },
    async create(task, receipt) {
      const key = store.prefix + 'create:' + digest(receipt);
      if (values.has(key)) return tasks.get(values.get(key));
      values.set(key, task.id); tasks.set(task.id, task); return task;
    },
    async mutate(id, receipt, fn) {
      const task = await store.get(id), key = id + ':' + receipt;
      if (!mutations.has(key)) { mutations.add(key); fn(task); }
      return task;
    }};
  return {store, tasks};
}

test('one process exception creates one repair report and keeps the process running despite further exceptions', async t => {
  const target = new EventEmitter(), reports = [], logs = [], exits = [];
  const handler = installFaultHandler({target, env: {AGENT_MODE: 'worker', OPENAI_API_KEY: 'hidden-api-key'},
    report: async value => { reports.push(value); return {id: 'repair-task'}; },
    log: value => logs.push(value), exit: code => exits.push(code), timeoutMs: 500});
  t.after(() => handler.dispose());
  target.emit('unhandledRejection', new Error('Broken callback hidden-api-key'));
  target.emit('uncaughtException', new Error('Second exception'));
  await delay(0);
  target.emit('uncaughtException', new Error('Third exception'));
  assert.equal(reports.length, 1); assert.equal(reports[0].component, 'worker');
  assert.match(reports[0].details, /\[REDACTED\]/);
  assert.equal(JSON.stringify(logs).includes('hidden-api-key'), false);
  assert.deepEqual(exits, []); assert.equal(logs.at(-1).taskId, 'repair-task');
  await handler.report(new Error('Caught IMAP exception'), 'imap_poll'); assert.equal(reports.length, 1);
});

test('a broken or stalled exception reporter does not recurse or stop the process', async t => {
  for (const report of [async () => { throw Error('Unavailable'); }, () => new Promise(() => {})]) {
    const target = new EventEmitter(), exits = [], logs = [];
    const handler = installFaultHandler({target, report, env: {}, timeoutMs: 10,
      exit: code => exits.push(code), log: value => logs.push(value)});
    t.after(() => handler.dispose()); target.emit('uncaughtException', new Error('Bug'));
    await delay(25); assert.deepEqual(exits, []);
    assert.equal(logs.filter(e => e.event === 'process_exception').length, 1);
  }
});

test('repair task is controller-attributed, targets codemowers/agent and deduplicates process reports', async () => {
  const {store, tasks} = fixture(); tasks.set('original-task', {id: 'original-task'});
  const report = {component: 'worker', processId: 'execution-1', executionId: 'execution-1', sourceTaskId: 'original-task',
    kind: 'uncaughtException', details: 'TypeError: broken callback'};
  const first = await createRepairTask(store, 'ns', report);
  const second = await createRepairTask(store, 'ns', report);
  assert.equal(first.id, second.id); assert.equal(tasks.size, 2);
  assert.equal(first.repository, 'codemowers/agent'); assert.equal(first.origin, 'controller');
  assert.equal(first.creator.type, 'controller'); assert.equal(first.pending.length, 1);
  assert.equal(first.failureReport.sourceTaskId, 'original-task');
  assert.match(first.objective, /do not modify unrelated tasks or auto-merge/);
  assert.equal(first.failureReport.occurrenceCount, 1);
  assert.equal(faultDetails('Bearer secretValue and https://user:pass@example.test', {}).includes('secretValue'), false);
});

test('controller deduplicates faults across workers/restarts and different kinds from one process', async () => {
  const {store, tasks} = fixture();
  const report = {component: 'worker', processId: 'worker-1', kind: 'uncaughtException',
    details: 'Error: kill EACCES\n    at process.kill (node:internal/process/per_thread:277:13)'};
  const [a, b] = await Promise.all([createRepairTask(store, 'ns', report),
    createRepairTask(store, 'ns', {...report, processId: 'worker-2'})]);
  assert.equal(a.id, b.id); assert.equal(tasks.size, 1); assert.equal(a.failureReport.occurrenceCount, 2);
  const different = await createRepairTask(store, 'ns', {...report, kind: 'worker_turn', details: 'A completely different exception'});
  assert.equal(different.id, a.id); assert.equal(tasks.size, 1);
  const restarted = await createRepairTask(store, 'ns', {...report, processId: 'worker-3', details: report.details.replace('277:13', '278:25')});
  assert.equal(restarted.id, a.id); assert.equal(tasks.size, 1); assert.equal(a.failureReport.occurrenceCount, 3);
});

test('different controller exceptions share one active repair task but get keyed follow-ups', async () => {
  const {store, tasks} = fixture();
  const imap = {component: 'controller', processId: 'controller-imap', kind: 'imap_connect', details: 'Error: Unexpected close'};
  const fetchFailure = {component: 'controller', processId: 'controller-fetch', kind: 'uncaughtException', details: 'TypeError: fetch failed'};
  const first = await createRepairTask(store, 'ns', imap);
  const second = await createRepairTask(store, 'ns', fetchFailure);
  assert.equal(first.id, second.id);
  assert.equal(tasks.size, 1);
  assert.equal(first.failureReport.fingerprints.length, 2);
  assert.equal(first.failureReport.occurrences.length, 2);
  assert.equal(first.pending.length, 2);
  assert.match(first.events.at(-1).content, /new controller exception/);
  await createRepairTask(store, 'ns', {...fetchFailure, processId: 'controller-fetch-restart'});
  assert.equal(first.failureReport.occurrenceCount, 3);
  assert.equal(first.pending.length, 2);
  const worker = await createRepairTask(store, 'ns', {component: 'worker', processId: 'worker-unrelated',
    kind: 'worker_turn', details: 'Worker-specific failure'});
  assert.notEqual(worker.id, first.id);
  assert.equal(tasks.size, 2);
  first.phase = 'Completed'; first.desiredState = 'Paused';
  const later = await createRepairTask(store, 'ns', {...imap, processId: 'controller-later'});
  assert.notEqual(later.id, first.id);
  assert.equal(tasks.size, 3);
});

test('repair worker faults attach to the root repair task without new tasks, prompts or state changes', async () => {
  const {store, tasks} = fixture();
  const root = await createRepairTask(store, 'ns', {component: 'controller', kind: 'imap_poll', processId: 'controller-1', details: 'IMAP failure'});
  root.desiredState = 'Paused'; root.phase = 'Paused'; const prompts = root.pending.length;
  const worker = await createRepairTask(store, 'ns', {component: 'worker', kind: 'uncaughtException', processId: 'worker-1', sourceTaskId: root.id, details: 'kill EACCES'});
  assert.equal(worker.id, root.id); assert.equal(tasks.size, 1); assert.equal(root.pending.length, prompts);
  assert.equal(root.desiredState, 'Paused'); assert.equal(root.failureReport.occurrenceCount, 2);
});

test('IMAP fault details retain safe operation/status and redact secrets without splitting the fingerprint', () => {
  const error = Object.assign(new Error('Command failed'), {agentImapOperation: {command: 'UID FETCH SOURCE', uid: 220},
    responseStatus: 'NO', responseText: 'Server refused hidden-password'});
  const details = faultDetails(error, {password: 'hidden-password'});
  assert.match(details, /UID FETCH SOURCE/); assert.match(details, /IMAP response status: NO/);
  assert.equal(details.includes('hidden-password'), false);
  assert.equal(faultFingerprint({component: 'controller', details}), faultFingerprint({component: 'controller', details: error.stack}));
});
