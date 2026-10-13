import {test} from 'node:test';
import assert from 'node:assert/strict';
import {newTask,reserve,changeState,blockExecution} from '../src/domain.js';
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
