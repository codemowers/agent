import http from 'node:http';
import {ImapFlow} from 'imapflow';

function sentFolder(entries, explicit) {
  const selectable = entry => !entry.flags?.has('\\Noselect') && !entry.flags?.has('\\NonExistent');
  const matches = entries.filter(entry => selectable(entry) && (explicit
    ? entry.path === explicit
    : entry.specialUse === '\\Sent' || entry.flags?.has('\\Sent')));
  if (matches.length !== 1) throw new Error(explicit ? 'configured Sent folder unavailable' : 'unique Sent folder unavailable');
  return matches[0].path;
}

export function createSentArchiveServer({host, port, tls, username, password, folder, Client = ImapFlow}) {
  const secure = tls === 'implicit' || (tls === 'auto' && port === 993);
  const withClient = async action => {
    const client = new Client({host, port, secure, doSTARTTLS: tls !== 'disabled' && !secure,
      auth: {user: username, pass: password}, logger: false,
      connectionTimeout: 15000, socketTimeout: 30000});
    try {
      await client.connect();
      const target = sentFolder(await client.list(), folder);
      return await action(client, target);
    } finally {
      try { await client.logout(); } catch { client.close(); }
    }
  };
  return http.createServer(async (req, res) => {
    if (req.url !== '/sent/ready' && req.url !== '/sent/append') {
      res.writeHead(404).end();
      return;
    }
    if (req.url === '/sent/ready' && req.method !== 'GET' || req.url === '/sent/append' && req.method !== 'POST') {
      res.writeHead(405).end();
      return;
    }
    try {
      let raw;
      if (req.method === 'POST') {
        const chunks = [];
        let size = 0;
        for await (const chunk of req) {
          size += chunk.length;
          if (size > 256 * 1024) {
            res.writeHead(413).end();
            return;
          }
          chunks.push(chunk);
        }
        raw = Buffer.concat(chunks);
        if (!raw.length) {
          res.writeHead(400).end();
          return;
        }
      }
      const target = await withClient(async (client, path) => {
        if (raw && !await client.append(path, raw, ['\\Seen'])) throw new Error('Sent APPEND failed');
        return path;
      });
      res.writeHead(200, {'content-type': 'application/json'}).end(JSON.stringify({folder: target}));
    } catch (error) {
      console.error(JSON.stringify({event: 'sent_archive_failed', code: error.code || 'UNKNOWN'}));
      res.writeHead(503).end();
    }
  });
}
