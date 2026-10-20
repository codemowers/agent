import {test} from 'node:test';
import assert from 'node:assert/strict';
import {runtimeConfig, turnMetrics} from '../src/task-worker.js';

test('runtime configuration applies only newer bounded updates', () => {
  const update = {type: 'config', generation: 5, idleSeconds: 0, model: 'claude-sonnet'};
  assert.equal(runtimeConfig(update, 4), update);
  assert.equal(runtimeConfig(update, 5), null);
  assert.throws(() => runtimeConfig({...update, idleSeconds: -1}, 4));
  assert.throws(() => runtimeConfig({...update, model: 42}, 4));
  assert.deepEqual(runtimeConfig({...update, disabledMcps: ['devices']}, 4).disabledMcps, ['devices']);
  assert.throws(() => runtimeConfig({...update, disabledMcps: ['bad name']}, 4));
});

test('turn metrics preserve zero counts and normalize CLI usage', () => {
  assert.deepEqual(turnMetrics(1234, {input_tokens: 10, output_tokens: 0, cached_input_tokens: 3}, 800), {
    durationMs: 1234, apiDurationMs: 800, inputTokens: 10, outputTokens: 0, cacheReadTokens: 3,
  });
  assert.deepEqual(turnMetrics(1, {input_tokens: -1, output_tokens: '4'}), {durationMs: 1});
  assert.deepEqual(turnMetrics(0, null), {durationMs: 0});
});
