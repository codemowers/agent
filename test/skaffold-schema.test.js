import test from 'node:test';
import assert from 'node:assert/strict';
import {stringify} from 'yaml';
import {SkaffoldSchema} from '../src/skaffold-schema.js';

test('namespace Kaniko example passes the full pinned CLI schema and uses the provisioned Secret', async () => {
  const subject = new SkaffoldSchema();
  const example = await subject.example({status: 'available', namespace: 'tenant', registry: 'zot.registry.example.test',
    skaffold: {defaultRepo: 'zot.registry.example.test/agent-smith', kubeContext: 'cluster.example.test/tenant', cluster: {namespace: 'tenant', dockerConfig: {secretName: 'zot-registry-cluster-push-kaniko'}}}}, 'example');
  assert.equal((await subject.validate(stringify(example.config))).valid, true);
  assert.deepEqual(example.config.manifests.rawYaml, ['k8s/*.yaml']);
  assert.equal(example.config.build.cluster.dockerConfig.secretName, 'zot-registry-cluster-push-kaniko');
  assert.equal(example.command, 'skaffold run');
  assert.equal(example.environment.SKAFFOLD_KUBE_CONTEXT, 'cluster.example.test/tenant');
  assert.equal(example.config.deploy.kubeContext, undefined);
  assert.equal(example.environment.SKAFFOLD_DEFAULT_REPO, 'zot.registry.example.test/agent-smith');
  const fragment = await subject.get('ClusterDetails');
  assert.equal(fragment.version, 'v2.18.0');
  assert.ok(fragment.schema.properties.dockerConfig);
  assert.ok(fragment.referencedDefinitions.includes('DockerConfig'));
  await assert.rejects(subject.get('../../secrets'), /Unknown Skaffold/);
});

test('offline Skaffold validation rejects the observed invented manifest fields and unsafe YAML', async () => {
  const subject = new SkaffoldSchema();
  const base = {apiVersion: 'skaffold/v4beta11', kind: 'Config', deploy: {kubectl: {manifests: ['k8s/*.yaml']}}};
  const result = await subject.validate(stringify(base));
  assert.equal(result.valid, false);
  assert.ok(result.errors.some(error => error.unknownField === 'manifests'));
  for (const content of ['apiVersion: [', 'kind: Config\nkind: Config', 'kind: !custom Config',
    'apiVersion: skaffold/v4beta11\nkind: Config\nmetadata: &m {name: example}\nbuild: *m']) {
    assert.equal((await subject.validate(content)).valid, false);
  }
  const valid = 'apiVersion: skaffold/v4beta11\nkind: Config\n';
  assert.equal((await subject.validate(valid + '---\n' + valid)).valid, true);
  await assert.rejects(subject.validate('x'.repeat(64001)), /64000/);
});
