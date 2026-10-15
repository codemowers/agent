import {test} from 'node:test';
import assert from 'node:assert/strict';
import {toolEvents} from '../src/tool-events.js';

import {newTask,reserve,workerEvent,activity,publicTask} from '../src/domain.js';
import {groupAgentMessages} from '../web/conversation.js';
import {sharedTask} from '../src/sharing.js';
import {mcpPayloadEnabled} from '../src/mcp-payload.js';

test('Claude tool requests/completions retain commands without tool output or credentials', () => {
  const event = {type:'assistant',message:{content:[{type:'tool_use',id:'call',name:'Bash',input:{command:'kubectl get pods -n llm'}}]}};
  const [request]=toolEvents('claude',event);
  assert.equal(request.command,'kubectl get pods -n llm');assert.equal(request.toolState,'running');
  event.message.content[0].input.command='kubectl create secret generic registry --from-literal=password=new-value';
  assert.equal(toolEvents('claude',event)[0].command,'[Credential-handling command hidden]');
  const [done]=toolEvents('claude',{type:'user',message:{content:[{type:'tool_result',tool_use_id:'call',is_error:true,content:'very secret output'}]}});
  assert.equal(done.toolState,'failed');assert.ok(!JSON.stringify(done).includes('very secret'));
});
test('Codex commands are redacted and exit outcomes recorded', () => {
  const [entry]=toolEvents('codex',{type:'item.completed',item:{id:'cmd',type:'command_execution',command:'echo sensitive-value',exit_code:2,aggregated_output:'private output'}},['sensitive-value']);
  assert.equal(entry.command,'echo [REDACTED]');assert.equal(entry.exitCode,2);assert.equal(entry.toolState,'failed');
  assert.ok(!JSON.stringify(entry).includes('private output'));
});
test('tool status updates stay in one agent response without duplicating entries or losing active prompts', () => {
  const task=newTask('ns',{type:'user'},'Build');const run=reserve(task);
  workerEvent(task,run.id,{summary:'Building'});
  workerEvent(task,run.id,{kind:'tool',toolId:'command',name:'Bash',command:'go test ./...',toolState:'running'});
  workerEvent(task,run.id,{kind:'tool',toolId:'command',toolState:'completed',exitCode:0});
  workerEvent(task,run.id,{summary:'Tests passed'});
  const groups=groupAgentMessages(task.events.filter(e=>e.actor.type==='agent'));
  assert.equal(groups.length,1);assert.equal(groups[0].tools.length,1);
  assert.equal(groups[0].tools[0].toolState,'completed');assert.equal(groups[0].content,'Building\n\nTests passed');
  for(let i=0;i<1100;i++)activity(task,{type:'agent'},'Progress','controller');
  assert.ok(task.events.some(e=>e.id===run.currentPrompts[0]));assert.equal(task.events.length,1000);
});

test('native Read/Write metadata identifies the file without forwarding its contents or secret arguments', () => {
  const task = newTask('ns', {type: 'user'}, 'Read config'); const run = reserve(task);
  const [request] = toolEvents('claude', {type: 'assistant', message: {content: [{type: 'tool_use',
    id: 'read', name: 'Read', input: {file_path: '/workspace/repo/skaffold.yaml', offset: 10, limit: 20, content: 'private content'}}]}});
  assert.equal(request.detail, 'file_path: /workspace/repo/skaffold.yaml · offset: 10 · limit: 20');
  workerEvent(task, run.id, request);
  workerEvent(task, run.id, toolEvents('claude', {type: 'user', message: {content: [{type: 'tool_result', tool_use_id: 'read', content: 'private output'}]}})[0]);
  assert.equal(task.events.find(event => event.toolName === 'Read').toolDetail, request.detail);
  assert.doesNotMatch(JSON.stringify(request), /private content|private output/);
  const [hidden] = toolEvents('claude', {type: 'assistant', message: {content: [{type: 'tool_use', id: 'write', name: 'Write',
    input: {file_path: '/tmp/sensitive-value', content: 'private content'}}]}}, ['sensitive-value']);
  assert.equal(hidden.detail, undefined);
});

test('MCP payloads require explicit enablement and stay out of normal and shared Task responses', () => {
  assert.equal(mcpPayloadEnabled('false'), false);
  assert.equal(mcpPayloadEnabled('true'), true);
  const task = newTask('ns', {type: 'user'}, 'Inspect'); const run = reserve(task);
  const request = {type: 'item.started', item: {id: 'mcp-1', type: 'mcp_tool_call', server: 'driftmower', tool: 'query_logs',
    arguments: {namespace: 'demo', token: 'private-token', query: '{"password":"hidden-value","limit":3}'}}};
  assert.equal(toolEvents('codex', request)[0].mcpRequest, undefined);
  workerEvent(task, run.id, toolEvents('codex', request, [], true)[0]);
  workerEvent(task, run.id, toolEvents('codex', {type: 'item.completed', item: {...request.item,
    result: {content: [{type: 'text', text: 'Bearer private-token'}]}}}, ['private-token'], true)[0]);
  const event = task.events.find(item => item.toolName === 'mcp__driftmower__query_logs');
  assert.equal(event.mcpRequest.token, '[REDACTED]');
  assert.equal(event.mcpRequest.query.password, '[REDACTED]');
  assert.doesNotMatch(JSON.stringify(event.mcpResponse), /private-token/);
  assert.equal(publicTask(task).events.find(item => item.id === event.id).mcpPayloadAvailable, true);
  assert.ok(!JSON.stringify(publicTask(task)).includes('mcpRequest'));
  assert.ok(!JSON.stringify(sharedTask(task)).includes('mcpResponse'));
});

test('Claude MCP results are captured only for known MCP tool IDs', () => {
  const ids = new Set(['mcp-call']);
  const results = toolEvents('claude', {type: 'user', message: {content: [
    {type: 'tool_result', tool_use_id: 'mcp-call', content: [{type: 'text', text: '{"ok":true}'}]},
    {type: 'tool_result', tool_use_id: 'shell-call', content: 'private shell output'},
  ]}}, [], true, ids);
  assert.deepEqual(results[0].mcpResponse, [{type: 'text', text: {ok: true}}]);
  assert.equal(results[1].mcpResponse, undefined);
});
