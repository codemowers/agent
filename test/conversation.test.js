import {test} from 'node:test';
import assert from 'node:assert/strict';
import {groupAgentMessages} from '../web/conversation.js';

test('agent progress accumulates through its result without merging turns or authors', () => {
  const agent = (kind, content, executionId = 'run') => ({actor: {type: 'agent'}, kind, content, executionId});
  const events = [agent('progress', 'First'), agent('progress', 'Second'),
    {...agent('result', 'Done'), metrics: {outputTokens: 42}}, agent('progress', 'Next turn'),
    {actor: {type: 'user'}, kind: 'prompt', content: 'Follow up'},
    agent('progress', 'Working'), agent('progress', 'Another execution', 'other')];
  const original = structuredClone(events);
  const grouped = groupAgentMessages(events);
  assert.equal(grouped.length, 5);
  assert.equal(grouped[0].content, 'First\n\nSecond\n\nDone');
  assert.equal(grouped[0].kind, 'result');
  assert.equal(grouped[0].metrics.outputTokens, 42);
  assert.equal(grouped[1].content, 'Next turn');
  assert.deepEqual(events, original);
});

test('historical messages with no execution identity remain separate', () => {
  const entry = {actor: {type: 'agent'}, kind: 'progress', content: 'Hello'};
  assert.equal(groupAgentMessages([entry, entry]).length, 2);
});
