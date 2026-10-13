import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp, readFile, writeFile, rm} from 'node:fs/promises';
import {join} from 'node:path';
import {tmpdir} from 'node:os';
import {prepareWorkspace} from '../src/workspace.js';

test('workspace refreshes owned Skaffold defaults without overwriting repository configuration or specifications', async t => {
  const cwd = await mkdtemp(join(tmpdir(), 'agent-workspace-'));
  t.after(() => rm(cwd, {recursive: true, force: true}));
  await writeFile(join(cwd, '.gitignore'), 'node_modules/\n');
  await writeFile(join(cwd, 'AGENTS.md'), '# Existing instructions\n');
  await writeFile(join(cwd, 'skaffold.env'), 'SKAFFOLD_DEFAULT_REPO=old\nCUSTOM_SETTING=keep\n');
  const task = {id: '11111111-1111-4111-8111-111111111111', objective: 'Build a thing'};
  const infrastructure = {status: 'available', namespace: 'tenant', registry: 'registry.test',
    skaffold: {defaultRepo: 'registry.test/agent-smith', kubeContext: 'cluster.test/tenant'}};
  const result = await prepareWorkspace(cwd, task, infrastructure, null, {mcpServers: {}});
  let env = await readFile(join(cwd, 'skaffold.env'), 'utf8');
  assert.match(env, /SKAFFOLD_DEFAULT_REPO="registry.test\/agent-smith"/);
  assert.match(env, /SKAFFOLD_KUBE_CONTEXT="cluster.test\/tenant"/);
  assert.match(env, /CUSTOM_SETTING=keep/);
  assert.equal(result.environment, 'skaffold.env');
  assert.match(await readFile(join(cwd, '.gitignore'), 'utf8'), /node_modules\/[\s\S]*\/skaffold.env/);
  const specification = await readFile(join(cwd, result.specification), 'utf8');
  task.objective = 'New requirement';
  await prepareWorkspace(cwd, task, {...infrastructure, skaffold: {...infrastructure.skaffold, defaultRepo: 'registry.test'}}, null, {mcpServers: {}});
  assert.equal(await readFile(join(cwd, result.specification), 'utf8'), specification);
  env = await readFile(join(cwd, 'skaffold.env'), 'utf8');
  assert.match(env, /SKAFFOLD_DEFAULT_REPO="registry.test"/);
  assert.equal((env.match(/SKAFFOLD_DEFAULT_REPO=/g) || []).length, 1);
  assert.equal((await readFile(join(cwd, 'AGENTS.md'), 'utf8')).split('## Agent workspace context').length, 2);
  await prepareWorkspace(cwd, task, {status: 'unavailable', namespace: 'tenant'}, null, {mcpServers: {}});
  assert.doesNotMatch(await readFile(join(cwd, 'skaffold.env'), 'utf8'), /SKAFFOLD_DEFAULT_REPO=/);
});
