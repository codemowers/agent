import test from 'node:test';
import assert from 'node:assert/strict';
import {randomUUID} from 'node:crypto';
import {createClient} from 'redis';
import {Store} from '../src/store.js';
import {newTask, prompt, reserve, changeState, deleteTask, workerEvent, claimNextTurn} from '../src/domain.js';
import {reconcile} from '../src/controller.js';

const user = {type: 'user', issuer: 'https://id.example/', subject: 'test-user'};
test('Redis atomic delivery, races, tenant isolation and startup recovery', {skip: !process.env.REDIS_TEST_URL}, async () => {
  const redis = createClient({url: process.env.REDIS_TEST_URL});
  await redis.connect();
  const store = new Store(redis, 'namespace-a', 'test-' + randomUUID());
  const other = new Store(redis, 'namespace-b', store.prefix);
  try {
    const a = await store.create(newTask('namespace-a', user, 'Build Etherpad'), 'request-1');
    const repeated = await store.create(newTask('namespace-a', user, 'Retry'), 'request-1');
    assert.equal(repeated.id, a.id);
    assert.equal((await store.list()).length, 1);
    await assert.rejects(other.get(a.id), error => error.status === 404);
    await Promise.all(Array.from({length: 5}, (_, i) => store.mutate(a.id, 'comment-' + i,
      current => prompt(current, user, 'Followup ' + i, 'webui'))));
    assert.equal((await store.get(a.id)).pending.length, 6);
    await store.mutate(a.id, 'comment-0', current => prompt(current, user, 'Duplicate', 'webui'));
    assert.equal((await store.get(a.id)).pending.length, 6);
    const current = await store.get(a.id);
    await assert.rejects(store.mutate(a.id, 'pause-old', task => changeState(task, 'Paused', user), current.revision - 1),
      error => error.status === 409);
    const reserved = await store.mutate(a.id, 'reserve', reserve), run = reserved.active, calls = [];
    const pod = {metadata: {name: run.podName, uid: 'live-pod', labels: {
      'agents.codemowers.io/task-id': a.id, 'agents.codemowers.io/execution-id': run.id}},
    status: {phase: 'Running'}};
    const kube = {leader: async () => true, get: async kind => kind === 'pod' ? pod : null,
      run: async args => { calls.push(args); return ''; }, ensure: async () => assert.fail('Live Pod was recreated')};
    await reconcile(store, kube, {});
    assert.equal((await store.get(a.id)).active.podUID, 'live-pod');
    pod.metadata.uid = 'replacement-pod';
    const before = calls.length;
    await reconcile(store, kube, {});
    assert.equal((await store.get(a.id)).reason, 'WorkerIdentityRequiresReview');
    assert.equal((await store.get(a.id)).desiredState, 'Paused');
    assert.equal(calls.length, before);
    pod.metadata.uid = 'live-pod';
    await store.mutate(a.id, 'reviewed', task => changeState(task, 'Active', user));
    await store.mutate(a.id, 'pause', task => changeState(task, 'Paused', user));
    await reconcile(store, kube, {});
    assert.ok(calls.some(args => args[0] === 'delete' && args[1] === 'pod'));
    pod.status = {phase: 'Failed', containerStatuses: [{name: 'agent', state: {terminated: {exitCode: 143}}}]};
    kube.run = async args => {
      calls.push(args);
      return args[0] === 'logs' ? JSON.stringify({type: 'agent_event', executionId: run.id, sequence: 1,
        kind: 'result', result: {exitCode: 0, summary: 'Worker claims success'}}) : '';
    };
    await reconcile(store, kube, {});
    const finished = await store.get(a.id);
    assert.equal(finished.phase, 'Paused'); assert.equal(finished.lastExecution.exitCode, 143);
    assert.equal(finished.active, null);
    assert.equal(finished.pending.length, 6);
    await store.mutate(a.id, 'delete', task => deleteTask(task, user));
    await reconcile(store, kube, {release: 'agent'});
    await assert.rejects(store.get(a.id), error => error.status === 404);
    assert.ok(calls.some(args => args[0] === 'delete' && args[1] === 'pvc'));
    const replacement = await store.create(newTask('namespace-a', user, 'New task'), 'request-1');
    assert.notEqual(replacement.id, a.id);
    await store.mutate(replacement.id, 'reserve-new', reserve);
    const executionId = (await store.get(replacement.id)).active.id;
    await store.mutate(replacement.id, 'turn-done', task => workerEvent(task, executionId,
      {kind:'turn_result',sequence:2,result:{exitCode:0,summary:'Done'}}));
    await store.mutate(replacement.id, 'next-prompt', task => prompt(task,user,'Followup','webui'));
    await Promise.all(Array.from({length:5},(_,i)=>store.mutate(replacement.id,'claim-'+i,
      task=>claimNextTurn(task,executionId))));
    const resident=await store.get(replacement.id);
    assert.equal(resident.active.currentPrompts.length,1);
    assert.equal(resident.pending.length,0);
  } finally {
    for await (const keys of redis.scanIterator({MATCH: store.prefix + '*', COUNT: 100})) await redis.del(keys);
    await redis.quit();
  }
});
