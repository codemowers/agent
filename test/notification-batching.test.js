import test from 'node:test';
import assert from 'node:assert/strict';
import {newTask, prompt, reserve, controllerActor, workerEvent, claimNextTurn, changeState, publicTask, activity} from '../src/domain.js';
import {sharedTask} from '../src/sharing.js';

const user = {type: 'user', name: 'sam'}, external = {type: 'external', name: 'Webhook'};
const automatic = task => prompt(task, controllerActor, 'Review new notifications.', 'controller', {automatic: true});
const clock = t => t.mock.timers.enable({apis: ['Date'], now: Date.parse('2026-10-13T12:00:00Z')});

test('first automatic wakeup starts immediately and suppression survives restart', t => {
  clock(t);
  const task = newTask('ns', external, 'Investigate event', 'webhook');
  const deadline = task.notificationSuppressUntil;
  assert.equal(Date.parse(deadline) - Date.now(), 180000);
  assert.equal(reserve(task).currentPrompts.length, 1);
  t.mock.timers.tick(30000);
  const restored = JSON.parse(JSON.stringify(task));
  activity(restored, external, 'Notification email', 'email', {kind: 'event'});
  assert.equal(automatic(restored), null);
  assert.deepEqual(restored.pending, []);
  assert.equal(restored.notificationSuppressUntil, deadline);
  t.mock.timers.tick(149999);
  assert.equal(automatic(restored), null);
  t.mock.timers.tick(1);
  assert.ok(automatic(restored));
  assert.equal(Date.parse(restored.notificationSuppressUntil) - Date.now(), 180000);
});

test('warm workers run the first notification immediately and ignore follow-ups for three minutes', t => {
  clock(t);
  const task = newTask('ns', user, 'Work'), run = reserve(task);
  workerEvent(task, run.id, {kind: 'turn_result', sequence: 1, result: {exitCode: 0, summary: 'Done'}});
  const first = automatic(task);
  claimNextTurn(task, run.id);
  assert.deepEqual(task.active.currentPrompts, [first.id]);
  assert.equal(task.pending.length, 0);
  t.mock.timers.tick(30000);
  assert.equal(automatic(task), null);
  assert.equal(task.pending.length, 0);
  t.mock.timers.tick(150000);
  const next = automatic(task);
  assert.deepEqual(task.pending, [next.id]);
});

test('human prompts and task email replies bypass suppression', t => {
  clock(t);
  for (const reply of [task => prompt(task, user, 'Do it now.', 'webui'),
    task => prompt(task, controllerActor, 'A participant replied to the task email.', 'controller')]) {
    const task = newTask('ns', external, 'Notification', 'webhook');
    const immediate = reply(task);
    assert.ok(reserve(task).currentPrompts.includes(immediate.id));
    assert.equal(automatic(task), null);
  }
});

test('pause and cancel retain authority while suppression remains fixed', t => {
  clock(t);
  const task = newTask('ns', external, 'Notification', 'webhook');
  const deadline = task.notificationSuppressUntil;
  changeState(task, 'Paused', user);
  assert.equal(reserve(task), undefined);
  t.mock.timers.tick(60000);
  assert.equal(automatic(task), null);
  assert.equal(task.notificationSuppressUntil, deadline);
  changeState(task, 'Active', user);
  assert.ok(reserve(task));
  const cancelled = newTask('ns', external, 'Notification', 'webhook');
  changeState(cancelled, 'Cancelled', user);
  assert.equal(reserve(cancelled), undefined);
});

test('read-only views show immediate prompts without suppression metadata', t => {
  clock(t);
  const task = newTask('ns', external, 'Notification', 'webhook');
  const view = sharedTask(task), summary = publicTask(task, false);
  assert.equal(view.notificationSuppressUntil, undefined);
  assert.equal(view.events[0].automatic, true);
  assert.equal(summary.notificationSuppressUntil, undefined);
  assert.equal(summary.events, undefined);
});
