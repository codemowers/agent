import {test} from 'node:test';
import assert from 'node:assert/strict';
import {newTask,reserve,changeState,blockExecution,deleteTask} from '../src/domain.js';
import {reconcile} from '../src/controller.js';

test('pause deletes the live worker before any potentially failing log retrieval', async () => {
  const actor={type:'user'},task=newTask('ns',actor,'Build');const execution=reserve(task);
  changeState(task,'Paused',actor);Object.assign(execution,{podPhase:'Running',podReady:true});
  const pod={metadata:{name:execution.podName,labels:{'agents.codemowers.io/task-id':task.id,'agents.codemowers.io/execution-id':execution.id}},
    status:{phase:'Running',conditions:[{type:'Ready',status:'True'}],containerStatuses:[{name:'agent',state:{running:{}}}]}};
  const calls=[];
  const kube={leader:async()=>true,get:async()=>pod,run:async args=>{calls.push(args);if(args[0]==='logs')throw Error('Logs unavailable');}};
  const receipts=new Set();
  const store={list:async()=>[task],mutate:async(id,request,fn)=>{if(!receipts.has(request)){receipts.add(request);fn(task);}return task;}};
  await reconcile(store,kube,{});
  assert.deepEqual(calls,[['delete','pod',execution.podName,'--wait=false']]);
  await reconcile(store,kube,{});
  assert.equal(task.events.filter(e=>e.kind==='execution_stop').length,1);
  assert.equal(task.events.find(e=>e.kind==='execution_stop').content,'Stopping worker');
});

test('waiting for confirmation keeps the live worker; explicit pause still deletes it', async () => {
  const task = newTask('ns', {type: 'user'}, 'Build'), execution = reserve(task);
  blockExecution(task, execution.id, 'Confirm before proceeding');
  const pod = {metadata: {name: execution.podName, uid: 'worker-uid', labels: {
    'agents.codemowers.io/task-id': task.id, 'agents.codemowers.io/execution-id': execution.id}},
  status: {phase: 'Running', conditions: [{type: 'Ready', status: 'True'}],
    containerStatuses: [{name: 'agent', state: {running: {}}}]}};
  const calls = [];
  const kube = {leader: async () => true, get: async () => pod, run: async args => {
    calls.push(args);
    if (args[0] === 'logs') return JSON.stringify({type: 'agent_event', executionId: execution.id,
      sequence: 1, kind: 'turn_result', result: {exitCode: 0, summary: 'Awaiting confirmation', completedAt: new Date().toISOString()}});
  }};
  const receipts = new Set(), store = {list: async () => [task], get: async () => task,
    mutate: async (id, request, fn) => { if (!receipts.has(request)) { receipts.add(request); fn(task); } return task; }};
  await reconcile(store, kube, {});
  assert.ok(!calls.some(args => args[0] === 'delete'));
  assert.equal(task.active.state, 'Idle'); assert.equal(task.reason, 'AwaitingConfirmation');
  assert.ok(Date.parse(task.active.idleExpiresAt) > Date.now() + 3500000);
  changeState(task, 'Paused', {type: 'user'});
  await reconcile(store, kube, {});
  assert.ok(calls.some(args => args[0] === 'delete' && args[1] === 'pod'));
});

test('failed worker pod remains after reconciliation and is removed with its task', async () => {
  const actor = {type: 'user'}, task = newTask('ns', actor, 'Build');
  const execution = reserve(task), pod = {metadata: {name: execution.podName,
    finalizers: ['agents.codemowers.io/result-recorded', 'other.example/cleanup'], labels: {
      'agents.codemowers.io/task-id': task.id, 'agents.codemowers.io/execution-id': execution.id}},
  status: {phase: 'Failed', containerStatuses: [{name: 'agent', state: {terminated: {exitCode: 1}}}]}};
  const calls = [], store = {list: async () => [task], get: async () => task,
    mutate: async (id, request, fn) => { fn(task); return task; }, remove: async () => {}};
  const kube = {leader: async () => true, get: async kind => kind === 'pod' ? pod : null,
    run: async args => { calls.push(args); return ''; }};
  await reconcile(store, kube, {release: 'agent'});
  assert.equal(task.lastExecution.state, 'Failed');
  assert.equal(task.lastExecution.result.summary, 'Worker stopped without a final result; inspect execution logs.');
  assert.ok(calls.some(args => args[0] === 'patch' && JSON.parse(args.at(-1)).metadata.finalizers.includes('other.example/cleanup')));
  assert.ok(!calls.some(args => args[0] === 'delete' && args[1] === 'pod'));
  calls.length = 0;
  await reconcile(store, kube, {release: 'agent'});
  assert.ok(!calls.some(args => args[0] === 'delete' && args[1] === 'pod'));
  deleteTask(task, actor);
  calls.length = 0;
  await reconcile(store, kube, {release: 'agent'});
  assert.ok(calls.some(args => args[0] === 'delete' && args[1] === 'pod,configmap'
    && args.includes(`agents.codemowers.io/task-id=${task.id}`)));
});

test('successful worker without a final report has its pod cleaned up', async () => {
  const task = newTask('ns', {type: 'user'}, 'Build'), execution = reserve(task);
  const pod = {metadata: {name: execution.podName, labels: {
    'agents.codemowers.io/task-id': task.id, 'agents.codemowers.io/execution-id': execution.id}},
  status: {phase: 'Succeeded', containerStatuses: [{name: 'agent', state: {terminated: {exitCode: 0}}}]}};
  const calls = [], store = {list: async () => [task], get: async () => task,
    mutate: async (id, request, fn) => { fn(task); return task; }};
  const kube = {leader: async () => true, get: async () => pod,
    run: async args => { calls.push(args); return ''; }};
  await reconcile(store, kube, {release: 'agent'});
  assert.equal(task.lastExecution.state, 'Succeeded');
  assert.ok(calls.some(args => args[0] === 'delete' && args[1] === 'pod'));
});
