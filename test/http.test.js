import test from 'node:test';
import assert from 'node:assert/strict';
import {agentFetch, agentUserAgent} from '../src/http.js';
import {responseJson} from '../web/http.js';

test('proxy text errors are readable and recoverable without exposing the response body', async () => {
  await assert.rejects(responseJson(new Response('no available server', {status: 503})),
    error => error.transient && error.message.includes('HTTP 503') && !error.message.includes('Unexpected token'));
  assert.deepEqual(await responseJson(new Response('{"ok":true}')), {ok: true});
  await assert.rejects(responseJson(new Response('{"error":"Task changed"}', {status: 409})),
    error => !error.transient && error.message === 'Task changed');
  await assert.rejects(responseJson(new Response('not JSON')), /invalid response/);
});

test('harness HTTP requests identify the agent and retain request headers', async t => {
  const original = globalThis.fetch;
  t.after(() => { globalThis.fetch = original; });
  let seen;
  globalThis.fetch = async (input, options) => { seen = {input, options}; return {ok: true}; };
  const request = new Request('https://example.test/path', {headers: {Accept: 'application/json'}});
  await agentFetch(request, {headers: {Authorization: 'Bearer token', 'User-Agent': 'other-client'}});
  assert.equal(seen.input, request);
  assert.equal(seen.options.headers.get('User-Agent'), agentUserAgent);
  assert.equal(seen.options.headers.get('Accept'), 'application/json');
  assert.equal(seen.options.headers.get('Authorization'), 'Bearer token');
  assert.equal(request.headers.get('User-Agent'), null);
});
