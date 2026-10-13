import test from 'node:test';
import assert from 'node:assert/strict';
import {agentIdentity, agentActor, displayIdentity} from '../src/identity.js';

test('configured agent identity attributes worker messages and exposes a public avatar without the mailbox address', () => {
  const env = {AGENT_USERNAME: 'agent-smith', AGENT_NAME: 'Agent Smith', AGENT_EMAIL: 'Agent.Smith@system.gov'};
  const actor = agentActor('execution', env), display = displayIdentity(env);
  assert.equal(actor.type, 'agent'); assert.equal(actor.name, 'Agent Smith');
  assert.equal(actor.subject, 'agent-smith'); assert.equal(actor.executionId, 'execution');
  assert.match(display.avatarHash, /^[a-f0-9]{64}$/);
  assert.equal(display.name, actor.name); assert.equal(display.username, actor.subject);
  assert.equal(JSON.stringify(display).includes('@'), false);
  assert.equal(agentActor('execution', {AGENT_USERNAME: 'another'}).name, 'another');
});

test('unconfigured agents default to Agent Smith', () => {
  assert.deepEqual(agentIdentity({}), {harness: 'codemowers/agent', username: 'agent-smith',
    name: 'Agent Smith', email: 'agent.smith@system.gov'});
});
