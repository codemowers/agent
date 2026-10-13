import test from 'node:test';
import assert from 'node:assert/strict';
import {setTimeout as delay} from 'node:timers/promises';
import {watchTurnControl} from '../src/turn-control.js';

test('worker turn control ignores stale prompt IDs, interrupts once and cannot signal a later turn after closing', async () => {
  let response = {interrupt: {promptIds: ['previous']}}, interrupted = 0;
  const control = await watchTurnControl({endpoint: 'https://controller/task', token: 'private-token', promptIds: ['current'],
    intervalMs: 5, interrupt: () => interrupted++, request: async (url, options) => {
      assert.equal(url, 'https://controller/task/control'); assert.equal(options.headers.Authorization, 'Bearer private-token');
      return {ok: true, json: async () => response};
    }});
  assert.equal(interrupted, 0);
  response = {interrupt: {promptIds: ['current']}}; await delay(30);
  assert.equal(interrupted, 1); await delay(20); assert.equal(interrupted, 1); control.close();
  let resolve, calls = 0;
  const late = await watchTurnControl({endpoint: 'https://controller/task', token: '', promptIds: ['next'], intervalMs: 5,
    interrupt: () => interrupted++, request: async () => ({ok: true, json: async () => calls++ === 0 ? {}
      : new Promise(done => { resolve = done; })})});
  await delay(20); late.close(); resolve({interrupt: {promptIds: ['next']}});
  await delay(20); assert.equal(interrupted, 1);
});
