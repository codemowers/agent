import test from 'node:test';
import assert from 'node:assert/strict';
import {SharedLinks} from '../src/sharing.js';
import {Problem} from '../src/domain.js';

test('anonymous share reads expose only one task’s view, need no link records and respect revocation/deletion without expiry', async () => {
  const records = new Map(); let now = 1000000;
  const task = {id: 'task', title: 'Shared task', creator: {type: 'user', name: 'Lauri', issuer: 'private-issuer', subject: 'private-subject'},
    events: [{id: 'message', kind: 'prompt', content: 'Hello', actor: {type: 'user', name: 'Lauri', email: 'private-email'}}],
    active: {token: 'execution-secret', state: 'Running', podName: 'agent-worker-test', private: 'hidden'},
    lastExecution: {token: 'old-secret', podName: 'old-pod', result: {private: 'hidden'}}, pending: [], namespace: 'hidden-namespace'};
  const store = {prefix: 'tenant:', redis: {set: async (key, value, options) => {
    throw new Error('No per-link Redis writes expected');
  }, get: async key => records.get(key)}, get: async id => {
    if (id !== task.id) throw new Problem(404, 'missing'); return task;
  }};
  store.mutate = async (id, receipt, fn) => { const task = await store.get(id); fn(task); return task; };
  store.list = async () => [task];
  const options = {now: () => now, mode: 'signed', key: 'test-sharing-key-that-is-long-enough'};
  const subject = new SharedLinks(store, options);
  const link = await subject.create(task.id);
  assert.equal(link.token.split('.')[1].length, 43);
  assert.equal(records.size, 0); assert.equal(link.expiresAt, undefined);
  const view = await subject.read(link.token);
  assert.equal(view.task.title, task.title); assert.equal(view.task.active.podName, 'agent-worker-test');
  assert.doesNotMatch(JSON.stringify(view), /execution-secret|old-secret|private-issuer|private-subject|private-email|hidden-namespace/);
  await assert.rejects(new SharedLinks({...store, prefix: 'other:'}, options).read(link.token), error => error.status === 404);
  await assert.rejects(new SharedLinks(store, {...options, key: 'another-key-that-is-long-enough-here'}).read(link.token), error => error.status === 404);
  await assert.rejects(subject.list(), error => error.status === 404);
  const publicShares = new SharedLinks(store, {...options, mode: 'public'});
  assert.equal((await publicShares.list()).length, 1);
  assert.equal((await publicShares.publicRead(task.id)).task.id, task.id);
  await assert.rejects(new SharedLinks(store).read(link.token), error => error.status === 404);
  task.shareEpoch = 1;
  await assert.rejects(subject.read(link.token), error => error.status === 404);
  const next = await subject.create(task.id);
  task.deleteRequested = true;
  await assert.rejects(subject.read(next.token), error => error.status === 404);
  task.deleteRequested = false; now += 7 * 86400000;
  assert.equal((await subject.read(next.token)).task.id, task.id);
  await assert.rejects(subject.read('task'), error => error.status === 404);
});
