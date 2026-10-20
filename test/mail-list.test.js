import {test} from 'node:test';
import assert from 'node:assert/strict';
import {Readable} from 'node:stream';
import {createMailListServer} from '../mailbox/list.js';

test('mailbox listing pages metadata and reads one message without marking it seen', async t => {
  const calls = [];
  class Client {
    async connect() {}
    async logout() {}
    async list() { return [{path: 'INBOX', flags: new Set()}]; }
    async mailboxOpen(path, options) { calls.push({path, options}); return {exists: 12, uidValidity: 5}; }
    async fetchAll(range) {
      calls.push(range);
      return [{uid: 81, envelope: {from: [{address: 'one@example.test'}], subject: 'One'}, internalDate: new Date('2026-10-19T00:00:00Z')},
        {uid: 82, envelope: {from: [{address: 'two@example.test'}], subject: 'Two'}, internalDate: new Date('2026-10-19T00:01:00Z')}];
    }
    async fetchOne(uid) {
      return {uid: Number(uid), envelope: {from: [{name: 'Two', address: 'two@example.test'}], subject: 'Two', messageId: '<two@example.test>'},
        internalDate: new Date('2026-10-19T00:01:00Z'), bodyStructure: {type: 'multipart/mixed', childNodes: [
          {part: '1', type: 'text/html'}, {part: '2', type: 'text/plain'},
          {part: '3', type: 'application/pdf', disposition: 'attachment', dispositionParameters: {filename: 'a.pdf'}, size: 9}]}};
    }
    async download(uid, part, options) {
      calls.push({download: uid, part, options});
      return {content: Readable.from([Buffer.from('Hello from two')])};
    }
  }
  const server = createMailListServer({host: 'mail.example.test', port: 993, tls: 'auto', username: 'user', password: 'secret', Client});
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(() => new Promise(resolve => server.close(resolve)));
  const url = `http://127.0.0.1:${server.address().port}`;
  const page = await (await fetch(`${url}/messages`, {method: 'POST', body: JSON.stringify({folder: 'INBOX', limit: 2})})).json();
  assert.equal(page.nextBeforeSequence, 11);
  assert.deepEqual(page.messages.map(message => message.uid), [82, 81]);
  const message = await (await fetch(`${url}/message`, {method: 'POST', body: JSON.stringify({uid: 82})})).json();
  assert.equal(message.text, 'Hello from two');
  assert.equal(message.from, 'Two <two@example.test>');
  assert.deepEqual(message.attachments, [{filename: 'a.pdf', type: 'application/pdf', size: 9}]);
  assert.deepEqual(calls.at(-1), {download: '82', part: '2', options: {uid: true}});
  assert.ok(calls.filter(call => call.options).every(call => call.path === undefined || call.options.readOnly));
  assert.equal((await fetch(`${url}/message`, {method: 'POST', body: JSON.stringify({uid: 0})})).status, 400);
});
