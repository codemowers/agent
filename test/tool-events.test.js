import {test} from 'node:test';
import assert from 'node:assert/strict';
import {toolEvents} from '../src/tool-events.js';

import {newTask,reserve,workerEvent,activity} from '../src/domain.js';
import {groupAgentMessages} from '../web/conversation.js';

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
