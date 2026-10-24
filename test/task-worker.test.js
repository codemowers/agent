import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp, mkdir, readFile, writeFile, rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {EventEmitter} from 'node:events';
import {DatabaseSync} from 'node:sqlite';
import {seedTaskContext, nativeSessionExists, brokerConnect, boundedTail, codexMcpArgs, mcpToolCalls, toolArguments, opencodeConfig, codexSessionModel, creditExhausted} from '../src/task-worker.js';

test('Turn text keeps its end within the byte bound', () => {
  assert.equal(boundedTail('short'), 'short');
  const text = 'ä'.repeat(5000) + 'conclusion';
  const tail = boundedTail(text);
  assert.ok(Buffer.byteLength(tail) <= 8000);
  assert.ok(tail.startsWith('…ä') && tail.endsWith('conclusion'));
});

test('Task context preserves edits and native session recovery sees retained state', async () => {
  const root = await mkdtemp(join(tmpdir(), 'agent-task-worker-'));
  const previousName = process.env.TASK_NAME;
  const previousAgent = process.env.AGENT_NAME;
  process.env.TASK_NAME = 'test-task';
  process.env.AGENT_NAME = 'developer';
  try {
    const repo = join(root, 'repo');
    await mkdir(repo);
    await seedTaskContext(repo, {type: 'objective', body: 'Build the thing'});
    await writeFile(join(repo, 'specs', 'task-test-task.md'), 'Edited by user\n');
    await seedTaskContext(repo, {type: 'objective', body: 'Old objective replayed'});
    assert.equal(await readFile(join(repo, 'specs', 'task-test-task.md'), 'utf8'), 'Edited by user\n');
    assert.match(await readFile(join(repo, 'AGENTS.md'), 'utf8'), /topic branch/);

    const session = join(root, 'home', '.claude', 'projects', 'repo', 'session-1.jsonl');
    await mkdir(join(root, 'home', '.claude', 'projects', 'repo'), {recursive: true});
    await writeFile(session, '');
    assert.equal(await nativeSessionExists(root, 'claude', 'session-1'), true);
    assert.equal(await nativeSessionExists(root, 'claude', 'missing'), false);
  } finally {
    if (previousName === undefined) delete process.env.TASK_NAME; else process.env.TASK_NAME = previousName;
    if (previousAgent === undefined) delete process.env.AGENT_NAME; else process.env.AGENT_NAME = previousAgent;
    await rm(root, {recursive: true, force: true});
  }
});

test('Worker logs in to RabbitMQ with its rotating ServiceAccount token', async () => {
  const root = await mkdtemp(join(tmpdir(), 'agent-broker-token-'));
  const saved = {...process.env};
  const tokenFile = join(root, 'token');
  process.env.AMQP_URL = 'amqp://broker.ns.svc:5672/%2F';
  process.env.AGENT_RABBITMQ_TOKEN_FILE = tokenFile;
  try {
    await writeFile(tokenFile, 'first.jwt\n');
    let connected;
    const secrets = [];
    const connection = Object.assign(new EventEmitter(), {updateSecret: async secret => { secrets.push(secret.toString()); }});
    await brokerConnect(async url => { connected = url; return connection; }, 10);
    assert.equal(connected, 'amqp://:first.jwt@broker.ns.svc:5672/%2F');
    await writeFile(tokenFile, 'second.jwt\n');
    await new Promise(resolve => setTimeout(resolve, 40));
    connection.emit('close');
    assert.equal(secrets.at(-1), 'second.jwt');
  } finally {
    process.env = saved;
    await rm(root, {recursive: true, force: true});
  }
});

test('Codex MCP overrides use bare server names', () => {
  assert.deepEqual(codexMcpArgs({agent_operator: {url: 'http://127.0.0.1:1234/mcp'}}),
    ['-c', 'mcp_servers.agent_operator.url="http://127.0.0.1:1234/mcp"']);
});

test('MCP calls are tracked from both CLIs with masked, bounded arguments', () => {
  const claude = mcpToolCalls('claude');
  assert.equal(claude.update({type: 'assistant', message: {content: [
    {type: 'tool_use', id: 't1', name: 'mcp__driftmower__query_logs', input: {namespace: 'agents', token: 'x'}},
    {type: 'tool_use', id: 't2', name: 'Bash', input: {command: 'ls'}}]}}), true);
  claude.update({type: 'assistant', parent_tool_use_id: 't9', message: {content: [{type: 'tool_use', id: 't3', name: 'mcp__devices__device_shell', input: {}}]}});
  claude.update({type: 'user', message: {content: [{type: 'tool_result', tool_use_id: 't1', is_error: true}]}});
  assert.deepEqual(claude.list(), [{server: 'driftmower', tool: 'query_logs', args: '{"namespace":"agents","token":"***"}', status: 'failed'}]);
  const codex = mcpToolCalls('codex');
  codex.update({type: 'item.started', item: {id: 'i1', type: 'mcp_tool_call', server: 'agent_operator', tool: 'set_task_title', arguments: {title: 'Watch'}, status: 'in_progress'}});
  codex.update({type: 'item.completed', item: {id: 'i1', type: 'mcp_tool_call', server: 'agent_operator', tool: 'set_task_title', arguments: {title: 'Watch'}, status: 'completed'}});
  assert.deepEqual(codex.list(), [{server: 'agent_operator', tool: 'set_task_title', args: '{"title":"Watch"}', status: 'done'}]);
  const opencode = mcpToolCalls('opencode', ['agent', 'agent_operator']);
  assert.equal(opencode.update({type: 'tool_use', part: {type: 'tool', tool: 'bash', callID: 'c0', state: {status: 'completed', input: {command: 'ls'}}}}), false);
  opencode.update({type: 'tool_use', part: {type: 'tool', tool: 'agent_operator_set_task_title', callID: 'c1', state: {status: 'completed', input: {title: 'Watch'}}}});
  assert.deepEqual(opencode.list(), [{server: 'agent_operator', tool: 'set_task_title', args: '{"title":"Watch"}', status: 'done'}]);
  assert.equal(toolArguments({text: 'x'.repeat(600)}).length, 500);
});

test('OpenCode gets the model server and MCPs without the key', () => {
  const config = opencodeConfig({agent_operator: {type: 'http', url: 'http://127.0.0.1:1234/mcp'}}, 'gemma.gguf', 'http://llama:8080/v1');
  assert.equal(config.model, 'agent/gemma.gguf');
  assert.deepEqual(config.provider.agent.options, {baseURL: 'http://llama:8080/v1', apiKey: '{env:AGENT_MODEL_API_KEY}',
    headerTimeout: 10800000, chunkTimeout: 3600000});
  assert.deepEqual(config.mcp, {agent_operator: {type: 'remote', url: 'http://127.0.0.1:1234/mcp', enabled: true, timeout: 600000}});
});

test('OpenCode sessions are found in its database', async () => {
  const root = await mkdtemp(join(tmpdir(), 'agent-opencode-'));
  try {
    assert.equal(await nativeSessionExists(root, 'opencode', 'ses_1'), false);
    const dir = join(root, 'home', '.local', 'share', 'opencode');
    await mkdir(dir, {recursive: true});
    const db = new DatabaseSync(join(dir, 'opencode.db'));
    db.exec('CREATE TABLE session (id TEXT PRIMARY KEY)');
    db.prepare('INSERT INTO session (id) VALUES (?)').run('ses_1');
    db.close();
    assert.equal(await nativeSessionExists(root, 'opencode', 'ses_1'), true);
    assert.equal(await nativeSessionExists(root, 'opencode', 'ses_2'), false);
  } finally {
    await rm(root, {recursive: true, force: true});
  }
});

test('Codex reports the model of its latest turn from the session file', async () => {
  const root = await mkdtemp(join(tmpdir(), 'agent-codex-'));
  try {
    const dir = join(root, 'home', '.codex', 'sessions', '2026', '10', '24');
    await mkdir(dir, {recursive: true});
    await writeFile(join(dir, 'rollout-2026-10-24T10-00-00-thread-1.jsonl'), [
      {type: 'session_meta', payload: {id: 'thread-1'}},
      {type: 'turn_context', payload: {model: 'gpt-old'}},
      {type: 'turn_context', payload: {model: 'gpt-new'}},
    ].map(entry => JSON.stringify(entry)).join('\n') + '\n');
    assert.equal(await codexSessionModel(root, 'thread-1'), 'gpt-new');
    assert.equal(await codexSessionModel(root, 'missing'), '');
    assert.equal(await codexSessionModel(root, '../x'), '');
  } finally {
    await rm(root, {recursive: true, force: true});
  }
});

test('Provider credit failures exclude transient limits and connectivity errors', () => {
  for (const message of ['Credit balance is too low', 'insufficient_quota', 'insufficient credits', 'billing_hard_limit_reached', 'You exceeded your current quota']) assert.equal(creditExhausted(message), true, message);
  for (const message of ['rate_limit_error', 'Too many requests', 'Connection timed out', 'overloaded_error', 'invalid_api_key']) assert.equal(creditExhausted(message), false, message);
});
