import test from 'node:test';
import assert from 'node:assert/strict';
import {BuildCatalog} from '../src/builds.js';

test('namespace build discovery exposes registry references without credentials or other Secret reads', async () => {
  const config = {auths: {'zot.registry.example.test': {auth: 'sensitive-auth', password: 'sensitive-password'}}};
  const kube = {config: {namespace: 'tenant'}, get: async (kind, name) => {
    assert.equal(kind, 'secret'); assert.equal(name, 'zot-registry-cluster-push-kaniko');
    return {data: {'config.json': Buffer.from(JSON.stringify(config)).toString('base64')}};
  }};
  const result = await new BuildCatalog(kube).list();
  assert.equal(result.registry, 'zot.registry.example.test');
  assert.equal(result.namespace, 'tenant'); assert.equal(result.status, 'available');
  assert.equal(result.pushSecret.key, 'config.json');
  assert.equal(result.skaffold.cluster.namespace, 'tenant');
  assert.doesNotMatch(JSON.stringify(result), /sensitive-auth|sensitive-password/);
});

test('missing, malformed and ambiguous push configuration fail closed without leaking Secret contents', async () => {
  for (const secret of [null, {data: {}}, {data: {'config.json': Buffer.from('sensitive invalid JSON').toString('base64')}},
    ...[{auths: {one: {auth: 'sensitive'}, two: {auth: 'sensitive'}}}, {auths: {'https://user:password@registry': {auth: 'sensitive'}}},
      {auths: {'registry.example.test': {}}}].map(config => ({data: {'config.json': Buffer.from(JSON.stringify(config)).toString('base64')}}))]) {
    const result = await new BuildCatalog({config: {namespace: 'tenant'}, get: async () => secret}).list();
    assert.equal(result.status, 'unavailable'); assert.equal(result.registry, undefined);
    assert.doesNotMatch(JSON.stringify(result), /sensitive|user:password/);
  }
});

test('registry repository prefix defaults to identity and supports an explicit per-user registry override', async () => {
  const kube = {config: {namespace: 'tenant'}, get: async () => ({data: {'config.json': Buffer.from(JSON.stringify({
    auths: {'zot.example.test': {auth: 'dXNlcjpwYXNz'}}})).toString('base64')}})};
  for (const [env, expected] of [[{AGENT_USERNAME: 'agent-smith'}, 'agent-smith'],
    [{AGENT_USERNAME: 'agent-smith', AGENT_REGISTRY_PREFIX: ''}, ''], [{AGENT_REGISTRY_PREFIX: 'team/bots'}, 'team/bots']]) {
    const result = await new BuildCatalog(kube, env).list();
    assert.equal(result.repositoryPrefix, expected);
    assert.equal(result.skaffold.defaultRepo, 'zot.example.test' + (expected ? '/' + expected : ''));
  }
  for (const prefix of ['https://elsewhere', '../personal', 'User', 'team//bot']) {
    assert.equal((await new BuildCatalog(kube, {AGENT_REGISTRY_PREFIX: prefix}).list()).status, 'unavailable');
  }
  const denied = new BuildCatalog({...kube, get: async () => { throw new Error('Namespace credentials: private-password'); }});
  assert.doesNotMatch(JSON.stringify(await denied.list()), /private-password/);
});
