import test from 'node:test';
import assert from 'node:assert/strict';
import {DeviceCatalog} from '../src/devices.js';
import {EventEmitter} from 'node:events';

test('task device context exposes only selected IDs and safe connection details', async () => {
  const data = Buffer.from(JSON.stringify({selected: {label: 'Lab Pi'}, other: {label: 'Private laptop'}})).toString('base64');
  const kube = {get: async () => ({data: {'devices.json': data}})};
  const catalog = new DeviceCatalog(kube, {release: 'agent', origin: 'https://controller.example.test'});
  catalog._connections.set('selected', {hostname: 'unknown', connectedAt: '2026-10-14T10:00:00Z'});

  assert.deepEqual(await catalog.taskContext(['selected', 'selected']), [
    {id: 'selected', label: 'Lab Pi', hostname: null, connected: true},
  ]);
  assert.deepEqual(await catalog.taskContext(['other']), [
    {id: 'other', label: 'Private laptop', hostname: null, connected: false},
  ]);
});

test('device hello exposes bounded OS details for the UI tooltip', async () => {
  const data = Buffer.from(JSON.stringify({selected: {label: 'Lab Pi'}})).toString('base64');
  const catalog = new DeviceCatalog({get: async () => ({data: {'devices.json': data}})},
    {release: 'agent', origin: 'https://controller.example.test'});
  const socket = new EventEmitter(); socket.close = () => {};
  catalog.handleConnection('selected', socket);
  socket.emit('message', JSON.stringify({type: 'hello', hostname: 'test-device', os: 'linux', arch: 'arm64', kernel: '6.8'}));
  await new Promise(resolve => setImmediate(resolve));
  const [device] = await catalog.list();
  assert.equal(device.hostname, 'test-device');
  assert.deepEqual(device.info, {os: 'linux', arch: 'arm64', kernel: '6.8'});
});
