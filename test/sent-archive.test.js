import {test} from 'node:test';
import assert from 'node:assert/strict';
import {createSentArchiveServer} from '../mailbox/sent-archive.js';

function fakeClient(entries, calls) {
  return class {
    async connect() { calls.push('connect'); }
    async list() { return entries; }
    async append(folder, raw, flags) {
      calls.push({folder, raw: raw.toString(), flags});
      return {uid: 4};
    }
    async logout() { calls.push('logout'); }
  };
}

async function listen(server, t) {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(() => new Promise(resolve => server.close(resolve)));
  return `http://127.0.0.1:${server.address().port}`;
}

test('Sent archive appends exact RFC 822 bytes with Seen to the unique Sent folder', async t => {
  const calls = [];
  const Client = fakeClient([{path: 'INBOX', flags: new Set()},
    {path: 'Sent Items', flags: new Set(['\\Sent']), specialUse: '\\Sent'}], calls);
  const server = createSentArchiveServer({host: 'mail.example.test', port: 993, tls: 'auto',
    username: 'user', password: 'secret', folder: '', Client});
  const url = await listen(server, t);
  const ready = await fetch(`${url}/sent/ready`);
  assert.equal(ready.status, 200);
  assert.deepEqual(await ready.json(), {folder: 'Sent Items'});
  const raw = 'Message-ID: <test@example.test>\r\n\r\nHello';
  const response = await fetch(`${url}/sent/append`, {method: 'POST', body: raw});
  assert.equal(response.status, 200);
  assert.deepEqual(await response.json(), {folder: 'Sent Items'});
  assert.deepEqual(calls.at(-2), {folder: 'Sent Items', raw, flags: ['\\Seen']});
});

test('Sent archive refuses ambiguous automatic folder selection', async t => {
  const calls = [];
  const Client = fakeClient([{path: 'Sent', flags: new Set(['\\Sent'])},
    {path: 'Other Sent', flags: new Set(['\\Sent'])}], calls);
  const server = createSentArchiveServer({host: 'mail.example.test', port: 993, tls: 'auto',
    username: 'user', password: 'secret', folder: '', Client});
  const url = await listen(server, t);
  assert.equal((await fetch(`${url}/sent/ready`)).status, 503);
  assert.ok(!calls.some(call => typeof call === 'object'));
});

test('explicit Sent folder resolves an otherwise ambiguous server', async t => {
  const calls = [];
  const Client = fakeClient([{path: 'Sent', flags: new Set(['\\Sent'])},
    {path: 'Agent Sent', flags: new Set(['\\Sent'])}], calls);
  const server = createSentArchiveServer({host: 'mail.example.test', port: 993, tls: 'auto',
    username: 'user', password: 'secret', folder: 'Agent Sent', Client});
  const url = await listen(server, t);
  const response = await fetch(`${url}/sent/ready`);
  assert.equal(response.status, 200);
  assert.deepEqual(await response.json(), {folder: 'Agent Sent'});
});
