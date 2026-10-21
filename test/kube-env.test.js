import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp, readFile, writeFile, rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {registryHost, inClusterKubeconfig, prepareKubeEnv} from '../src/kube-env.js';

test('registry host comes only from an unambiguous push credential', () => {
  assert.equal(registryHost(JSON.stringify({auths: {'zot.registry.example.test': {auth: 'x'}}})), 'zot.registry.example.test');
  assert.equal(registryHost(JSON.stringify({auths: {'https://zot.example.test:5000/': {}}})), 'zot.example.test:5000');
  assert.equal(registryHost(JSON.stringify({auths: {a: {}, b: {}}})), '');
  assert.equal(registryHost('{'), '');
});

test('kubeconfig targets the in-cluster API with the rotating token', () => {
  const config = inClusterKubeconfig({host: 'fd10::1', port: '443', namespace: 'team', dir: '/sa'});
  assert.equal(config.clusters[0].cluster.server, 'https://[fd10::1]:443');
  assert.equal(config.users[0].user.tokenFile, '/sa/token');
  assert.equal(config.contexts[0].context.namespace, 'team');
});

test('workers without an API token get no Kubernetes environment', async t => {
  const home = await mkdtemp(tmpdir() + '/kube-env-'); t.after(() => rm(home, {recursive: true, force: true}));
  const env = {};
  await prepareKubeEnv(home, env, home + '/missing');
  assert.deepEqual(env, {});
});

test('template settings win and Skaffold gets no implicit namespace', async t => {
  const dir = await mkdtemp(tmpdir() + '/kube-sa-'); t.after(() => rm(dir, {recursive: true, force: true}));
  await writeFile(dir + '/namespace', 'team'); await writeFile(dir + '/token', 't');
  const env = {KUBERNETES_SERVICE_HOST: '10.0.0.1', SKAFFOLD_DEFAULT_REPO: 'registry.example.test/custom'};
  await prepareKubeEnv(dir, env, dir);
  assert.equal(env.SKAFFOLD_NAMESPACE, undefined);
  assert.equal(env.SKAFFOLD_KUBE_CONTEXT, 'in-cluster');
  assert.equal(env.SKAFFOLD_DEFAULT_REPO, 'registry.example.test/custom');
  assert.equal(JSON.parse(await readFile(env.KUBECONFIG, 'utf8')).contexts[0].context.namespace, 'team');
});
