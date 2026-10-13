import test from 'node:test';
import assert from 'node:assert/strict';
import {createHash} from 'node:crypto';
import {probeRegistry} from '../src/registry.js';
import {BuildCatalog} from '../src/builds.js';

function registry({redirect = false, denied = false, deleteDenied = false, mismatch = false} = {}) {
  const calls = []; let manifest;
  const request = async (url, options) => {
    calls.push({url: String(url), ...options});
    assert.equal(url.origin, 'https://registry.test');
    assert.equal(options.redirect, 'error');
    assert.equal(options.headers.Authorization, 'Basic dXNlcjpwYXNz');
    if (options.method === 'POST') return new Response(null, {status: denied ? 403 : 202,
      headers: {Location: redirect ? 'https://untrusted.test/v2/upload' : '/v2/agent-smith/agent-connectivity-check/tenant/blobs/uploads/unique?state=opaque'}});
    if (options.method === 'PUT' && url.pathname.includes('/blobs/')) {
      assert.equal(url.searchParams.get('state'), 'opaque');
      const expected = 'sha256:' + createHash('sha256').update(options.body).digest('hex');
      assert.equal(url.searchParams.get('digest'), expected);
      const config = JSON.parse(options.body); assert.deepEqual(config.rootfs.diff_ids, []);
      return new Response(null, {status: 201});
    }
    if (options.method === 'PUT') { manifest = options.body; return new Response(null, {status: 201}); }
    if (options.method === 'GET') return new Response(mismatch ? '{}' : manifest);
    if (options.method === 'DELETE') return new Response(null, {status: deleteDenied ? 403 : 202});
    throw new Error('Unexpected request');
  };
  return {request, calls};
}

test('registry push verifies bytes and removes only its own unique manifest digest', async () => {
  const mock = registry();
  const result = await probeRegistry('registry.test', 'agent-smith/agent-connectivity-check/tenant', {auth: 'dXNlcjpwYXNz'}, mock.request);
  assert.equal(result.verified, true); assert.equal(result.cleanup, 'removed');
  assert.match(result.reference, /^registry.test\/agent-smith\/agent-connectivity-check\/tenant:check-/);
  const remove = mock.calls.at(-1);
  assert.equal(remove.method, 'DELETE');
  assert.match(new URL(remove.url).pathname, /\/manifests\/sha256:[a-f0-9]{64}$/);
  assert.doesNotMatch(JSON.stringify(result), /dXNlcjpwYXNz/);
});

test('registry rejects remote upload destinations, fails on auth/read-back errors and reports cleanup failure', async () => {
  for (const options of [{redirect: true}, {denied: true}, {mismatch: true}]) {
    const mock = registry(options);
    await assert.rejects(probeRegistry('registry.test', 'agent-smith/agent-connectivity-check/tenant', {auth: 'dXNlcjpwYXNz'}, mock.request));
    assert.ok(mock.calls.every(call => new URL(call.url).origin === 'https://registry.test'));
    if (options.mismatch) assert.equal(mock.calls.at(-1).method, 'DELETE');
  }
  const mock = registry({deleteDenied: true});
  const result = await probeRegistry('registry.test', 'agent-smith/agent-connectivity-check/tenant', {auth: 'dXNlcjpwYXNz'}, mock.request);
  assert.equal(result.verified, true); assert.equal(result.cleanup, 'unavailable'); assert.equal(result.cleanupStatus, 403);
  const kube = {config: {namespace: 'tenant'}, get: async () => ({data: {'config.json': Buffer.from(JSON.stringify({
    auths: {'registry.test': {auth: 'dXNlcjpwYXNz'}}})).toString('base64')}})};
  const check = await new BuildCatalog(kube, {AGENT_USERNAME: 'agent-smith'}, registry({denied: true}).request).probe();
  assert.equal(check.status, 'unavailable'); assert.equal(check.detail, 'Authentication rejected');
  assert.doesNotMatch(JSON.stringify(check), /dXNlcjpwYXNz/);
});
