import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mcpProbeContext, probeMcpEndpoint} from '../src/task-worker.js';

test('MCP probe initializes, then lists tools in the returned session', async () => {
  const calls = [];
  const result = await probeMcpEndpoint({url: 'https://mcp.example.test'}, async (_server, options) => {
    const request = JSON.parse(options.body);
    calls.push({method: request.method, session: options.headers['Mcp-Session-Id']});
    return new Response(JSON.stringify({jsonrpc: '2.0', id: request.id, result: {}}), {
      headers: {'Mcp-Session-Id': 'session-one'},
    });
  });
  assert.deepEqual(result, {state: 'available'});
  assert.deepEqual(calls, [{method: 'initialize', session: undefined}, {method: 'tools/list', session: 'session-one'}]);
});

test('MCP probe reports a bounded failure without exposing response content', async () => {
  const result = await probeMcpEndpoint({url: 'https://mcp.example.test'}, async () => new Response('secret details', {status: 503}));
  assert.deepEqual(result, {state: 'unavailable', detail: 'HTTP 503'});
});

test('probe context keeps bounded title, instructions and tool names', () => {
  const context = mcpProbeContext(
    {serverInfo: {name: 'agent-devices', title: ' Devices '}, instructions: 'Authorized remote devices:\n blueos (connected).'},
    {tools: [{name: 'list_devices'}, {name: 'bad name'}, {name: 'device_shell'}]});
  assert.deepEqual(context, {serverTitle: 'Devices', instructions: 'Authorized remote devices: blueos (connected).',
    tools: ['list_devices', 'device_shell']});
  assert.equal([...mcpProbeContext({instructions: 'x'.repeat(900)}, {}).instructions].length, 500);
  assert.deepEqual(mcpProbeContext({}, {}), {});
});
