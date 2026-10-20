import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp, mkdir, readFile, writeFile, rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {EventEmitter} from 'node:events';
import {seedTaskContext, nativeSessionExists, brokerConnect} from '../src/task-worker.js';

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
