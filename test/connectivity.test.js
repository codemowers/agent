import test from 'node:test';
import assert from 'node:assert/strict';
import {ConnectivityCatalog, checkConnection} from '../src/connectivity.js';
import {SetupCatalog} from '../src/setup.js';

test('startup checks authenticate once; reading snapshots or refreshing repository discovery never rechecks connectivity', async t => {
  const token = process.env.GH_TOKEN;
  process.env.GH_TOKEN = 'startup-test-token';
  t.after(() => { if (token === undefined) delete process.env.GH_TOKEN; else process.env.GH_TOKEN = token; });
  const counts = {github: 0, mcp: 0, agent: 0, inbound: 0, outbound: 0, registry: 0};
  const setup = new SetupCatalog({probeAgent: async () => {
    counts.agent++; return {protocolVersion: '2025-03-26', capabilities: {tools: {}}};
  }}, async (url, options) => {
    const path = new URL(url).pathname;
    const response = data => new Response(JSON.stringify(data), {headers: {'Content-Type': 'application/json'}});
    if (path === '/mcp') { counts.mcp++; assert.equal(options.headers.Authorization, 'Bearer mcp-test-token');
      return response({id: 1, result: {protocolVersion: '2025-03-26'}}); }
    if (path === '/user') { counts.github++; assert.equal(options.headers.Authorization, 'Bearer startup-test-token'); return response({login: 'agent'}); }
    if (['/user/repos', '/user/orgs'].includes(path)) return response([]);
    throw new Error('Unexpected request');
  }, async () => JSON.stringify({mcpServers: {codemowers: {url: 'https://mcp.test/mcp', headers: {Authorization: 'Bearer mcp-test-token'}}}}));
  const mailbox = {probeInbound: async () => { counts.inbound++; return {name: 'inbound-mail', status: 'available', checkedAt: new Date().toISOString()}; },
    probeOutbound: async () => { counts.outbound++; return {name: 'outbound-mail', status: 'available', checkedAt: new Date().toISOString()}; }};
  const catalog = new ConnectivityCatalog(setup, mailbox, {probe: async () => { counts.registry++;
    return {name: 'registry', status: 'available', checkedAt: new Date().toISOString()}; }});
  const [first, second] = await Promise.all([catalog.start(), catalog.start()]);
  assert.deepEqual(counts, {github: 1, mcp: 1, agent: 1, inbound: 1, outbound: 1, registry: 1});
  assert.equal(first, second);
  assert.deepEqual(await catalog.snapshot(), first);
  assert.ok(first.connections.every(connection => Number.isFinite(Date.parse(connection.checkedAt))));
  assert.ok(first.connections.filter(connection => connection.name !== 'driftmower').every(connection => connection.status === 'available'));
  assert.equal(first.connections.find(connection => connection.name === 'driftmower').status, 'disabled');
  assert.equal(JSON.stringify(first).includes('test-token'), false);
  // Explicit Refresh remains repository discovery, not a connectivity reprobe.
  await setup.list(true);
  assert.deepEqual(counts, {github: 2, mcp: 1, agent: 1, inbound: 1, outbound: 1, registry: 1});
  assert.deepEqual(await catalog.snapshot(), first);
  mailbox.checks = {'inbound-mail': {name: 'inbound-mail', status: 'unavailable', detail: 'Disconnected',
    source: 'operation', checkedAt: new Date().toISOString()}};
  assert.equal((await catalog.snapshot()).connections.find(connection => connection.name === 'inbound-mail').status, 'unavailable');
  assert.deepEqual(counts, {github: 2, mcp: 1, agent: 1, inbound: 1, outbound: 1, registry: 1});
});

test('auth rejection, deadlines and built-in MCP failures produce safe dated snapshots', async () => {
  const result = await checkConnection('github', 'success', async () => {
    throw Object.assign(new Error('Bearer secret token'), {upstreamStatus: 401});
  });
  assert.equal(result.status, 'unavailable');
  assert.equal(result.detail, 'Authentication rejected');
  assert.equal(JSON.stringify(result).includes('secret'), false);
  let closed = false;
  const timeout = await checkConnection('smtp', 'success', () => new Promise(() => {}), {timeoutMs: 5, onTimeout: () => { closed = true; }});
  assert.equal(timeout.detail, 'Connection check timed out'); assert.equal(closed, true);
  const setup = new SetupCatalog({probeAgent: async () => { throw new Error('secret storage failure'); }}, fetch,
    async () => '{"mcpServers":{}}');
  const agent = (await setup.mcps()).find(connection => connection.name === 'agent');
  assert.equal(agent.name, 'agent'); assert.equal(agent.status, 'unavailable');
  assert.equal(JSON.stringify(agent).includes('secret'), false);
});
