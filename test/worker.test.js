import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp, mkdir, writeFile, readFile, rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import http from 'node:http';
import {spawn} from 'node:child_process';
import {randomUUID} from 'node:crypto';

test('worker accepts a controller idle-timeout increase and resumes followups in the same process', async t => {
  const root = await mkdtemp(join(tmpdir(), 'agent-worker-test-'));
  t.after(() => rm(root, {recursive: true, force: true}));
  const bin = join(root, 'bin'); await mkdir(bin);
  await writeFile(join(bin, 'gh'), '#!/bin/sh\nexit 99\n', {mode: 0o755});
  await writeFile(join(bin, 'claude'), `#!/usr/bin/env node
const fs=require('node:fs');const p=require('node:path');
const args=process.argv.slice(2);const resume=args.indexOf('--resume');
const session=args[(resume>=0?resume:args.indexOf('--session-id'))+1];
const dir=p.join(process.env.HOME,'.claude/projects/test');fs.mkdirSync(dir,{recursive:true});
fs.writeFileSync(p.join(dir,session+'.jsonl'),'{}');
fs.appendFileSync(p.join(process.env.AGENT_STATE_DIR,'calls.jsonl'),JSON.stringify({resume:resume>=0,session,input:args[args.indexOf('-p')+1],instructions:args[args.indexOf('--append-system-prompt')+1]})+'\\n');
const answer=resume>=0?'Second answer':'First answer';
console.log(JSON.stringify({type:'assistant',session_id:session,message:{content:[{type:'text',text:answer}]}}));
console.log(JSON.stringify({type:'result',session_id:session,result:answer,duration_api_ms:25,usage:{input_tokens:10,output_tokens:resume>=0?234:123,cache_read_input_tokens:8}}));
`, {mode: 0o755});
  const sessionId = randomUUID(), executionId = randomUUID();
  const first = {id: randomUUID(), actor: {type: 'user'}, content: 'First prompt'};
  const second = {id: randomUUID(), actor: {type: 'user'}, content: 'Second prompt'};
  await writeFile(join(root, 'envelope.json'), JSON.stringify({sessionId, objective: 'Test task', prompts: [first],
    references: [{source: 'github', kind: 'issue', repository: 'example/sample', id: '42'}]}));
  await writeFile(join(root, 'instructions.md'), 'Instructions');
  await writeFile(join(root, 'mcp.json'), '{"mcpServers":{}}');
  let delivered = false;
  const server = http.createServer(async (req, res) => {
    assert.equal(req.headers.authorization, 'Bearer test-token');
    if (req.url.endsWith('/control')) { res.setHeader('content-type', 'application/json'); res.end('{"interrupt":null,"idleTimeoutSeconds":3}'); return; }
    if (req.url.endsWith('/context')) {
      assert.equal(req.method, 'GET');
      res.setHeader('content-type', 'application/json');
      res.end(JSON.stringify({infrastructure: {status: 'available', registry: 'zot.registry.example.test', namespace: 'test', skaffold: {defaultRepo: 'zot.registry.example.test/agent-smith'},
        pushSecret: {name: 'zot-registry-cluster-push-kaniko', key: 'config.json'}}, controls: {},
        devices: [{id: 'device-1', label: 'Lab Pi', hostname: null, connected: true}]})); return;
    }
    let body='';for await(const c of req)body+=c;
    assert.ok(JSON.parse(body).afterSequence >= 2);
    const prompts = delivered ? [] : [second];
    if (!delivered) await writeFile(join(root, 'instructions.md'), 'Updated instructions');
    delivered = true;
    res.setHeader('content-type','application/json');
    res.end(JSON.stringify({desiredState:'Active',session:{id:sessionId},prompts}));
  });
  await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
  t.after(()=>server.close());
  const child = spawn(process.execPath, ['--input-type=module', '-e', 'import {worker} from "./src/worker.js";await worker();'],
    {env:{...process.env, GH_TOKEN: '', PATH:bin+':'+process.env.PATH, HOME:join(root,'home'), AGENT_STATE_DIR:root,
      AGENT_TASK_ID:randomUUID(), AGENT_EXECUTION_ID:executionId, AGENT_REPORT_TOKEN:'test-token',
      AGENT_SERVICE_URL:`http://127.0.0.1:${server.address().port}`, AGENT_ENVELOPE_FILE:join(root,'envelope.json'),
      AGENT_INSTRUCTIONS_FILE:join(root,'instructions.md'),AGENT_MCP_CONFIG_FILE:join(root,'mcp.json'),
      AGENT_BACKEND:'claude',AGENT_MODEL:'mock',AGENT_IDLE_TIMEOUT_SECONDS:'0',ANTHROPIC_API_KEY:'test-key'},
      stdio:['ignore','pipe','pipe']});
  let output='',errors='';child.stdout.on('data',c=>output+=c);child.stderr.on('data',c=>errors+=c);
  t.after(()=>{if(child.exitCode===null)child.kill('SIGKILL');});
  const code=await new Promise(resolve=>child.on('close',resolve));
  assert.equal(code,0,errors);
  const calls=(await readFile(join(root,'calls.jsonl'),'utf8')).trim().split('\n').map(JSON.parse);
  assert.equal(calls.length,2);assert.equal(calls[0].resume,false);assert.equal(calls[1].resume,true);
  assert.equal(calls[0].session,calls[1].session);assert.match(calls[1].input,/Second prompt/);
  assert.deepEqual(calls.map(call=>call.instructions),['Instructions','Updated instructions']);
  assert.ok(calls.every(call=>call.input.includes('zot.registry.example.test') && call.input.includes('zot-registry-cluster-push-kaniko')));
  assert.ok(calls.every(call=>call.input.includes('"harness":"codemowers/agent"') && call.input.includes('Controller mailbox status:')));
  assert.ok(calls.every(call=>call.input.includes('"id":"device-1"') && call.input.includes('"connected":true')));
  assert.ok(calls.every(call=>call.input.includes('"primary":{"channel":"github_issue","url":"https://github.com/example/sample/issues/42"}')));
  const events=output.trim().split('\n').map(JSON.parse);
  assert.equal(events.filter(e=>e.kind==='turn_result').length,2);
  const turns=events.filter(e=>e.kind==='turn_result');
  assert.deepEqual(turns.map(e=>e.result.metrics.outputTokens),[123,234]);
  assert.equal(turns[0].result.metrics.inputTokens,10);
  assert.equal(turns[0].result.metrics.cacheReadTokens,8);
  assert.equal(turns[0].result.metrics.apiDurationMs,25);
  assert.ok(turns.every(e=>Number.isFinite(e.result.metrics.durationMs)&&e.result.metrics.durationMs>0));
  assert.equal(events.filter(e=>e.kind==='turn_started').length,2);
  assert.equal(events.find(e=>e.type==='agent_result').result.summary,'Second answer');
});

test('worker exits with a recorded failure when the selected model API has no credential', async t => {
  for (const [backend, missing, other] of [
    ['claude', 'ANTHROPIC_API_KEY', 'OPENAI_API_KEY'],
    ['codex', 'OPENAI_API_KEY', 'ANTHROPIC_API_KEY'],
  ]) {
    const root = await mkdtemp(join(tmpdir(), 'agent-worker-no-api-'));
    t.after(() => rm(root, {recursive: true, force: true}));
    const executionId = randomUUID();
    await writeFile(join(root, 'envelope.json'), JSON.stringify({sessionId: randomUUID()}));
    const child = spawn(process.execPath, ['--input-type=module', '-e', "import {worker} from './src/worker.js'; await worker();"],
      {env: {...process.env, AGENT_STATE_DIR: root, AGENT_ENVELOPE_FILE: join(root, 'envelope.json'),
        AGENT_EXECUTION_ID: executionId, AGENT_BACKEND: backend, [missing]: '', [other]: 'other-provider-only'},
      stdio: ['ignore', 'pipe', 'pipe']});
    let output = '', errors = '';
    child.stdout.on('data', chunk => output += chunk);
    child.stderr.on('data', chunk => errors += chunk);
    const code = await new Promise(resolve => child.on('close', resolve));
    assert.equal(code, 1, errors);
    const result = JSON.parse(await readFile(join(root, 'executions', executionId + '.result.json'), 'utf8'));
    assert.equal(result.exitCode, 1);
    assert.match(result.summary, new RegExp(missing));
    assert.ok(output.split('\n').filter(Boolean).map(JSON.parse).some(event => event.type === 'agent_result'));
  }
});
