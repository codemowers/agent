import {test} from 'node:test';
import assert from 'node:assert/strict';
import {createServer as createSMTPServer} from 'node:net';
import {createServer as createHTTPServer} from 'node:http';
import {spawn} from 'node:child_process';

async function listen(server, t) {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(() => new Promise(resolve => server.close(resolve)));
  return server.address().port;
}

async function unusedPort() {
  const server = createHTTPServer();
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const port = server.address().port;
  await new Promise(resolve => server.close(resolve));
  return port;
}

test('SMTP acceptance archives the same Message-ID and reports archive failure without resending', async t => {
  const received = [];
  const smtp = createSMTPServer(socket => {
    socket.write('220 local ESMTP\r\n');
    let pending = '';
    let dataMode = false;
    socket.on('data', chunk => {
      pending += chunk.toString();
      while (true) {
        if (dataMode) {
          const end = pending.indexOf('\r\n.\r\n');
          if (end < 0) return;
          received.push(pending.slice(0, end));
          pending = pending.slice(end + 5);
          dataMode = false;
          socket.write('250 2.0.0 accepted\r\n');
          continue;
        }
        const end = pending.indexOf('\r\n');
        if (end < 0) return;
        const line = pending.slice(0, end);
        pending = pending.slice(end + 2);
        if (/^EHLO /i.test(line)) socket.write('250-local\r\n250-AUTH PLAIN\r\n250 SIZE 1048576\r\n');
        else if (/^AUTH PLAIN /i.test(line)) socket.write('235 2.7.0 ok\r\n');
        else if (/^DATA$/i.test(line)) { dataMode = true; socket.write('354 end with dot\r\n'); }
        else if (/^QUIT$/i.test(line)) { socket.write('221 bye\r\n'); socket.end(); }
        else socket.write('250 ok\r\n');
      }
    });
  });
  const smtpPort = await listen(smtp, t);
  let archiveFails = false;
  const archived = [];
  const archive = createHTTPServer(async (req, res) => {
    if (req.url === '/sent/ready') { res.writeHead(200, {'content-type': 'application/json'}).end('{"folder":"Sent"}'); return; }
    const chunks = [];
    for await (const chunk of req) chunks.push(chunk);
    archived.push(Buffer.concat(chunks).toString());
    res.writeHead(archiveFails ? 503 : 200, {'content-type': 'application/json'}).end('{"folder":"Sent"}');
  });
  const archivePort = await listen(archive, t);
  const apiPort = await unusedPort();
  const child = spawn(process.execPath, ['mailbox/smtp.js'], {env: {...process.env,
    SMTP_HOST: '127.0.0.1', SMTP_PORT: String(smtpPort), SMTP_TLS: 'disabled',
    SMTP_USERNAME: 'user', SMTP_PASSWORD: 'password', SMTP_FROM_ADDRESS: 'sender@example.test',
    SMTP_SAVE_SENT: 'true', SMTP_API_PORT: String(apiPort),
    IMAP_ARCHIVE_URL: `http://127.0.0.1:${archivePort}`,
  }, stdio: 'ignore'});
  t.after(() => { child.kill(); });
  const endpoint = `http://127.0.0.1:${apiPort}`;
  for (let i = 0; i < 40; i++) {
    try { await fetch(endpoint); break; } catch { await new Promise(resolve => setTimeout(resolve, 50)); }
  }
  const send = async () => {
    const body = JSON.stringify({to: ['recipient@example.test'], subject: 'Test', text: 'Hello'});
    const response = await fetch(`${endpoint}/send`, {method: 'POST', body});
    assert.equal(response.status, 200);
    return response.json();
  };
  const first = await send();
  assert.equal(first.sentCopy.status, 'archived');
  assert.equal(first.sentCopy.folder, 'Sent');
  assert.equal(received.length, 1);
  assert.equal(archived.length, 1);
  assert.ok(received[0].includes(`Message-ID: ${first.messageId}`));
  assert.ok(archived[0].includes(`Message-ID: ${first.messageId}`));
  archiveFails = true;
  const second = await send();
  assert.equal(second.sentCopy.status, 'failed');
  assert.equal(received.length, 2);
});
