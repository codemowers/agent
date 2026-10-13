import test from 'node:test';
import assert from 'node:assert/strict';
import {signalChild} from '../src/child-signals.js';

const child = () => ({pid: 123, exitCode: null, signalCode: null});
test('denied process-group signals fall back to the child PID during stop and shutdown', () => {
  for (const signal of ['SIGINT', 'SIGTERM', 'SIGKILL']) {
    const calls = [];
    assert.equal(signalChild(child(), signal, {kill(pid, value) {
      calls.push([pid, value]); if (pid < 0) throw Object.assign(new Error('denied'), {code: 'EACCES'});
    }}), true);
    assert.deepEqual(calls, [[-123, signal], [123, signal]]);
  }
});
test('signal denial and disappearing children do not throw or trigger recursive exception handling', () => {
  const denied = [];
  assert.equal(signalChild(child(), 'SIGTERM', {kill() { throw Object.assign(new Error('denied'), {code: 'EPERM'}); }, denied: value => denied.push(value)}), false);
  assert.equal(denied.length, 1);
  assert.equal(signalChild(child(), 'SIGTERM', {kill() { throw Object.assign(new Error('gone'), {code: 'ESRCH'}); }}), false);
  assert.equal(signalChild({...child(), exitCode: 0}, 'SIGKILL', {kill() { throw Error('must not signal'); }}), false);
});
