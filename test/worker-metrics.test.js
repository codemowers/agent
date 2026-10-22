import {test} from 'node:test';
import assert from 'node:assert/strict';
import {createRegistry, serveMetrics, setReady} from '../src/metrics.js';
import {recordTurn} from '../src/task-worker.js';

test('registry renders counters and cumulative histogram buckets', () => {
  const registry = createRegistry();
  const counter = registry.counter('example_total', 'Example.');
  const histogram = registry.histogram('example_seconds', 'Example.', [1, 5]);
  recordTurn('codex', 'gpt-x', 'completed', {durationMs: 3000, inputTokens: 10, cacheReadTokens: 4}, histogram, counter);
  recordTurn('codex', 'gpt-x', 'completed', {durationMs: 500, inputTokens: 5}, histogram, counter);
  const text = registry.text();
  assert.match(text, /^example_total\{backend="codex",model="gpt-x",kind="input"\} 15$/m);
  assert.match(text, /^example_total\{backend="codex",model="gpt-x",kind="cache_read"\} 4$/m);
  assert.doesNotMatch(text, /kind="output"/);
  assert.match(text, /^example_seconds_bucket\{backend="codex",model="gpt-x",outcome="completed",le="1"\} 1$/m);
  assert.match(text, /^example_seconds_bucket\{backend="codex",model="gpt-x",outcome="completed",le="5"\} 2$/m);
  assert.match(text, /^example_seconds_bucket\{backend="codex",model="gpt-x",outcome="completed",le="\+Inf"\} 2$/m);
  assert.match(text, /^example_seconds_sum\{backend="codex",model="gpt-x",outcome="completed"\} 3.5$/m);
});

test('readiness follows the worker and metrics are served', async t => {
  const server = serveMetrics(0);
  await new Promise(resolve => server.once('listening', resolve));
  t.after(() => server.close());
  const url = `http://127.0.0.1:${server.address().port}`;
  setReady(false);
  assert.equal((await fetch(`${url}/readyz`)).status, 503);
  setReady(true);
  assert.equal((await fetch(`${url}/readyz`)).status, 200);
  assert.match(await (await fetch(`${url}/metrics`)).text(), /^agent_worker_idle_seconds_total \d/m);
});
