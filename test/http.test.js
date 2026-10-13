import {test} from 'node:test';
import assert from 'node:assert/strict';
import {responseJson} from '../web/http.js';

test('proxy text errors are readable and recoverable without exposing the response body', async () => {
  await assert.rejects(responseJson(new Response('no available server', {status: 503})),
    error => error.transient && error.message.includes('HTTP 503') && !error.message.includes('Unexpected token'));
  assert.deepEqual(await responseJson(new Response('{"ok":true}')), {ok: true});
  await assert.rejects(responseJson(new Response('{"error":"Task changed"}', {status: 409})),
    error => !error.transient && error.message === 'Task changed');
  await assert.rejects(responseJson(new Response('not JSON')), /invalid response/);
});
