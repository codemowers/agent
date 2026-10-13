import test from 'node:test';
import assert from 'node:assert/strict';
import {ModelCatalog, changeModel} from '../src/models.js';
import {newTask, reserve, finish, changeState} from '../src/domain.js';
import {Kube} from '../src/kube.js';

const actor = {type: 'user', issuer: 'https://issuer.test', subject: 'me'};
test('catalog offers only hosted Claude and Codex and validates CLI/provider selection', async () => {
  const catalog = new ModelCatalog({model: 'cloud-claude', codexModel: 'cloud-codex'});
  const listed = await catalog.list();
  assert.deepEqual(listed.models.map(model => model.provider), ['anthropic', 'openai']);
  assert.deepEqual(listed.warnings, []);
  assert.equal((await catalog.select()).model, 'cloud-claude');
  assert.equal((await catalog.select(undefined, 'codex')).model, 'cloud-codex');
  await assert.rejects(catalog.select('https://arbitrary-host', 'claude'));
  await assert.rejects(catalog.select('anthropic:default', 'codex'));
  await assert.rejects(catalog.select('openai:default', 'unknown'));
});

test('CLI/model changes require a stopped paused Task, reset native session, and retain workspace identity', () => {
  const task = newTask('tenant-a', actor, 'Build software'), id = task.id;
  const first = reserve(task);
  assert.match(first.podName, /^agent-worker-/);
  const model = {id: 'openai:default', provider: 'openai', backend: 'codex', model: 'cloud-codex',
    label: 'Codex', backends: ['codex']};
  assert.throws(() => changeModel(task, model, actor));
  changeState(task, 'Paused', actor);
  assert.throws(() => changeModel(task, model, actor));
  finish(task, first.id, {exitCode: 143, sessionId: first.sessionId});
  changeModel(task, model, actor);
  assert.equal(task.session, null); assert.equal(task.id, id);
  changeState(task, 'Active', actor);
  const second = reserve(task);
  assert.notEqual(second.sessionId, first.sessionId);
  assert.equal(second.sessionEpoch, 2);
  const kube = new Kube({release: 'agent', namespace: 'tenant-a', githubSecret: 'github', openaiSecret: 'openai-api-token', image: 'runner'});
  const env = kube.pod(task).spec.containers[0].env;
  assert.equal(env.find(e => e.name === 'OPENAI_API_KEY').valueFrom.secretKeyRef.name, 'openai-api-token');
  assert.ok(!env.some(e => e.name === 'ANTHROPIC_API_KEY'));
  assert.equal(env.find(e => e.name === 'AGENT_MODEL').value, 'cloud-codex');
  assert.equal(kube.pvc(task).metadata.name, 'agent-task-' + id);
});

test('Codex default does not inherit the Claude model or credential', () => {
  const task = newTask('tenant-a', actor, 'Build software');
  task.model = {id: 'openai:default', backend: 'codex', provider: 'openai', model: ''}; reserve(task);
  const kube = new Kube({release: 'agent', model: 'claude-cloud', openaiSecret: 'openai-api-token', githubSecret: 'github'});
  const env = kube.pod(task).spec.containers[0].env;
  assert.equal(env.find(e => e.name === 'AGENT_MODEL').value, '');
  assert.equal(env.find(e => e.name === 'OPENAI_API_KEY').valueFrom.secretKeyRef.name, 'openai-api-token');
  assert.ok(!env.some(e => e.name === 'ANTHROPIC_API_KEY'));
});

test('new workers mount kubectl matching the live API version and retain other image tools', async () => {
  const task = newTask('tenant-a', actor, 'Build software'); reserve(task);
  const kube = new Kube({release: 'agent', namespace: 'tenant-a', githubSecret: 'github', image: 'runner',
    toolImages: [{name: 'kubectl', image: 'outdated', binPath: 'bin'}, {name: 'utils', image: 'utilities', binPath: 'usr/bin'}]});
  let gitVersion = 'v1.36.3';
  kube.run = async args => {
    assert.deepEqual(args, ['version', '-o', 'json']);
    return JSON.stringify({serverVersion: {gitVersion}});
  };
  for (const version of ['v1.36.3', 'v1.37.1']) {
    gitVersion = version;
    const pod = await kube.workerPod(task), container = pod.spec.containers[0];
    assert.equal(pod.spec.volumes.find(volume => volume.name === 'tool-kubectl').image.reference,
      'registry.k8s.io/kubectl:' + version);
    assert.equal(pod.spec.volumes.find(volume => volume.name === 'tool-utils').image.reference, 'utilities');
    assert.equal(container.volumeMounts.find(mount => mount.name === 'tool-kubectl').subPath, 'bin');
    assert.match(container.env.find(env => env.name === 'PATH').value, /^\/opt\/tools\/kubectl\/bin:/);
  }
  gitVersion = undefined;
  await assert.rejects(kube.workerPod(task), /valid server version/);
});

test('controller Lease timestamps use Kubernetes MicroTime precision', async () => {
  const kube = new Kube({release: 'agent', instance: 'instance'});
  kube.get = async () => null;
  kube.create = async object => { assert.match(object.spec.renewTime, /\.\d{6}Z$/); };
  assert.equal(await kube.leader(), true);
});
