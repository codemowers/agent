import test from 'node:test';
import assert from 'node:assert/strict';
import {queuedDeliveries, turnMessage} from '../src/turn-queue.js';

const delivery = (body, count, redelivered = false) => ({
  fields: {messageCount: count, redelivered},
  content: Buffer.from(JSON.stringify({type: 'prompt', body})),
});

test('restart reads steering behind a redelivered request before starting a turn', async () => {
  const old = delivery('Implement squeezelite', 2, true);
  const event = delivery('Notification', 1);
  const steer = delivery('Use HTTP SSE instead', 0);
  const queue = [old, event, steer];
  const seen = [];
  const channel = {get: async (name, options) => {
    assert.equal(name, 'task');
    assert.deepEqual(options, {noAck: false});
    const next = queue.shift(); seen.push(next); return next || false;
  }};
  const batch = await queuedDeliveries(channel, 'task');
  assert.deepEqual(batch, [old, event, steer]);
  assert.equal(seen.length, 3);
  const prompt = turnMessage(batch.map(item => JSON.parse(item.content)));
  assert.match(prompt.body, /Read all inputs before acting/);
  assert.match(prompt.body, /Later instructions take precedence/);
  assert.ok(prompt.body.indexOf('Use HTTP SSE instead') > prompt.body.indexOf('Implement squeezelite'));
});

test('collection stops at the snapshot despite new incoming messages', async () => {
  const queue = [delivery('first', 1), delivery('second', 1), delivery('arrived later', 0)];
  assert.equal((await queuedDeliveries({get: async () => queue.shift()}, 'task')).length, 2);
  assert.equal(queue.length, 1);
});

test('empty queue, disappearing messages, and a single input are preserved', async () => {
  assert.deepEqual(await queuedDeliveries({get: async () => false}, 'task'), []);
  const first = delivery('only', 1);
  const queue = [first, false];
  assert.deepEqual(await queuedDeliveries({get: async () => queue.shift()}, 'task'), [first]);
  const message = {type: 'objective', body: 'original', messageId: 'one'};
  assert.equal(turnMessage([message]), message);
});

test('combined turns acknowledge every input ID in delivery order', () => {
 const message=turnMessage([{type:'prompt',messageId:'one',body:'First'}, {type:'prompt',messageId:'two',body:'Second'}]);
 assert.deepEqual(message.messageIds,['one','two']);
});
