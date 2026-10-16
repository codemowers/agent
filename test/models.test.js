import test from 'node:test';
import assert from 'node:assert/strict';
import {ModelCatalog, changeModel} from '../src/models.js';
import {newTask, reserve, finish, prompt, claimNextTurn} from '../src/domain.js';
import {Kube} from '../src/kube.js';

const actor = {type: 'user', issuer: 'https://issuer.test', subject: 'me'};
test('catalog discovers credential-accessible Claude and coding models and validates selection', async () => {
  const catalog = new ModelCatalog({model: 'cloud-claude', codexModel: 'cloud-codex'});
  const oldAnthropic = process.env.ANTHROPIC_API_KEY, oldOpenai = process.env.OPENAI_API_KEY;
  process.env.ANTHROPIC_API_KEY = 'anthropic-test'; process.env.OPENAI_API_KEY = 'openai-test';
  catalog.fetchModels = async (url, options) => {
    if (url.includes('anthropic')) {
      assert.equal(options.headers['x-api-key'], 'anthropic-test');
      return {ok: true, json: async () => ({data: [{id: 'claude-sonnet-5-5', display_name: 'Claude Sonnet 5.5'},
        {id: 'claude-sonnet-4-5-20250929'}]})};
    }
    assert.equal(options.headers.Authorization, 'Bearer openai-test');
    return {ok: true, json: async () => ({data: [{id: 'gpt-6.1-sol'}, {id: 'gpt-5.3-codex'}, {id: 'gpt-6.1-sol-search'}]})};
  };
  try { await catalog.refresh(); } finally {
    if (oldAnthropic === undefined) delete process.env.ANTHROPIC_API_KEY; else process.env.ANTHROPIC_API_KEY = oldAnthropic;
    if (oldOpenai === undefined) delete process.env.OPENAI_API_KEY; else process.env.OPENAI_API_KEY = oldOpenai;
  }
  const listed = await catalog.list();
  assert.deepEqual(listed.models.map(model => model.id),
    ['anthropic:default', 'openai:default', 'anthropic:claude-sonnet-5-5',
      'anthropic:claude-sonnet-4-5-20250929', 'openai:gpt-6.1-sol', 'openai:gpt-5.3-codex']);
  assert.deepEqual(listed.warnings, []);
  assert.equal((await catalog.select()).model, 'cloud-claude');
  assert.equal((await catalog.select(undefined, 'codex')).model, 'cloud-codex');
  assert.equal((await catalog.select('anthropic:claude-sonnet-5-5', 'claude')).model, 'claude-sonnet-5-5');
  await assert.rejects(catalog.select('https://arbitrary-host', 'claude'));
  await assert.rejects(catalog.select('anthropic:default', 'codex'));
  await assert.rejects(catalog.select('openai:default', 'unknown'));
});

test('transient discovery errors retain cached models for the affected provider', async () => {
  const catalog = new ModelCatalog({model: 'claude-default', codexModel: 'codex-default'});
  const oldAnthropic = process.env.ANTHROPIC_API_KEY, oldOpenai = process.env.OPENAI_API_KEY;
  process.env.ANTHROPIC_API_KEY = 'anthropic-test'; process.env.OPENAI_API_KEY = 'openai-test';
  catalog.fetchModels = async url => ({ok: true, json: async () => ({data: [
    {id: url.includes('anthropic') ? 'claude-sonnet-5-5' : 'gpt-6.1-sol'}]})});
  try {
    await catalog.refresh();
    catalog.fetchModels = async url => {
      if (url.includes('anthropic')) throw new Error('temporary outage');
      return {ok: true, json: async () => ({data: [{id: 'gpt-6.1-sol'}]})};
    };
    const listed = await catalog.refresh();
    assert.ok(listed.models.some(model => model.id === 'anthropic:claude-sonnet-5-5'));
    assert.ok(listed.models.some(model => model.id === 'openai:gpt-6.1-sol'));
    assert.match(listed.warnings[0], /temporary outage/);
  } finally {
    if (oldAnthropic === undefined) delete process.env.ANTHROPIC_API_KEY; else process.env.ANTHROPIC_API_KEY = oldAnthropic;
    if (oldOpenai === undefined) delete process.env.OPENAI_API_KEY; else process.env.OPENAI_API_KEY = oldOpenai;
  }
});

test('model change during active execution takes effect after the turn and retains workspace identity', () => {
  const task = newTask('tenant-a', actor, 'Build software'), id = task.id;
  const first = reserve(task);
  assert.match(first.podName, /^agent-worker-/);
  const model = {id: 'openai:default', provider: 'openai', backend: 'codex', model: 'cloud-codex',
    label: 'Codex', backends: ['codex']};
  changeModel(task, model, actor);
  assert.equal(task.pendingModel.id, model.id);
  assert.equal(task.active.model?.backend, first.model?.backend);
  task.active.state = 'Idle';
  prompt(task, actor, 'Use Codex next', 'webui');
  claimNextTurn(task, first.id);
  assert.equal(task.active.state, 'Idle');
  assert.equal(task.pending.length, 1);
  finish(task, first.id, {exitCode: 143, sessionId: first.sessionId});
  assert.equal(task.session, null); assert.equal(task.id, id);
  assert.equal(task.pendingModel, null);
  assert.equal(task.phase, 'Pending');
  const second = reserve(task);
  assert.notEqual(second.sessionId, first.sessionId);
  assert.equal(second.sessionEpoch, 2);
  const kube = new Kube({release: 'agent', namespace: 'tenant-a', githubSecret: 'github', openaiSecret: 'openai-api-token', image: 'runner'});
  const env = kube.pod(task).spec.containers[0].env;
  assert.equal(env.find(e => e.name === 'OPENAI_API_KEY').valueFrom.secretKeyRef.name, 'openai-api-token');
  assert.equal(env.find(e => e.name === 'OPENAI_API_KEY').valueFrom.secretKeyRef.optional, true);
  assert.equal(env.find(e => e.name === 'GH_TOKEN').valueFrom.secretKeyRef.optional, true);
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

test('Claude credential reference is optional so the worker can report a missing API', () => {
  const task = newTask('tenant-a', actor, 'Build software'); reserve(task);
  const kube = new Kube({release: 'agent', anthropicSecret: 'anthropic-api-token', githubSecret: 'github'});
  const env = kube.pod(task).spec.containers[0].env;
  assert.deepEqual(env.find(e => e.name === 'ANTHROPIC_API_KEY').valueFrom.secretKeyRef,
    {name: 'anthropic-api-token', key: 'token', optional: true});
});

test('new workers mount kubectl matching the live API version and retain other image tools', async () => {
  const task = newTask('tenant-a', actor, 'Build software'); reserve(task);
  const kube = new Kube({release: 'agent', namespace: 'tenant-a', githubSecret: 'github', image: 'runner',
    toolImages: [{name: 'kubectl', image: 'outdated', binPath: 'bin'}, {name: 'utils', image: 'utilities', binPath: 'usr/bin'},
      {name: 'go', image: 'golang:1.27.2-bookworm', binPath: 'usr/local/go', mountPath: '/opt/tools/go'},
      {name: 'helm', image: 'alpine/helm:3.22.0', binPath: 'usr/bin'}]});
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
    assert.equal(pod.spec.volumes.find(volume => volume.name === 'tool-go').image.reference, 'golang:1.27.2-bookworm');
    assert.equal(container.volumeMounts.find(mount => mount.name === 'tool-kubectl').subPath, 'bin');
    assert.deepEqual(container.volumeMounts.find(mount => mount.name === 'tool-go'),
      {name: 'tool-go', mountPath: '/opt/tools/go', subPath: 'usr/local/go', readOnly: true});
    const paths = container.env.find(env => env.name === 'PATH').value.split(':');
    assert.equal(paths[0], '/opt/tools/kubectl/bin');
    assert.ok(paths.indexOf('/usr/bin') < paths.indexOf('/opt/tools/helm/bin'));
    assert.ok(paths.includes('/opt/tools/go/bin'));
    assert.equal(container.env.find(env => env.name === 'GOROOT').value, '/opt/tools/go');
    assert.equal(container.env.find(env => env.name === 'GOPATH').value, '/workspace/home/go');
    assert.equal(container.env.find(env => env.name === 'GOTOOLCHAIN').value, 'local');
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
