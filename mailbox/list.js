import http from 'node:http';
import {ImapFlow} from 'imapflow';

const textLimit = 64 * 1024;

// Walks a BODYSTRUCTURE for the first inline text part (plain preferred) and
// the attachments, which are described but never returned.
export function messageParts(node, found = {attachments: []}) {
  if (!node) return found;
  if (node.childNodes?.length) {
    for (const child of node.childNodes) messageParts(child, found);
    return found;
  }
  const filename = node.dispositionParameters?.filename || node.parameters?.name || '';
  if (node.disposition === 'attachment' || filename) {
    if (found.attachments.length < 50) found.attachments.push({filename: filename.slice(0, 255), type: node.type || '', size: node.size || 0});
  } else if (node.type === 'text/plain' && !found.plain) found.plain = node;
  else if (node.type === 'text/html' && !found.html) found.html = node;
  return found;
}

function htmlText(html) {
  return html.replace(/<(script|style)[^>]*>[\s\S]*?<\/\1>/gi, ' ').replace(/<br\s*\/?>|<\/(p|div|tr|li)>/gi, '\n')
    .replace(/<[^>]+>/g, '').replace(/&nbsp;/g, ' ').replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&quot;/g, '"')
    .replace(/&#39;/g, "'").replace(/&amp;/g, '&').replace(/[ \t]+\n/g, '\n').replace(/\n{3,}/g, '\n\n').trim();
}

async function boundedText(stream) {
  const chunks = [];
  let size = 0;
  for await (const chunk of stream) {
    const buffer = Buffer.from(chunk);
    chunks.push(buffer.subarray(0, textLimit + 1 - size));
    size += buffer.length;
    if (size > textLimit) { stream.destroy?.(); break; }
  }
  return {text: Buffer.concat(chunks).toString('utf8').slice(0, textLimit), truncated: size > textLimit};
}

// Served on loopback to the Pod's mcp container, which authorizes callers.
export function createMailListServer({host, port, tls, username, password, Client = ImapFlow}) {
  const secure = tls === 'implicit' || (tls === 'auto' && port === 993);
  const withClient = async action => {
    const client = new Client({host, port, secure, doSTARTTLS: tls !== 'disabled' && !secure,
      auth: {user: username, pass: password}, logger: false,
      connectionTimeout: 15000, socketTimeout: 30000});
    try {
      await client.connect();
      return await action(client);
    } finally {
      try { await client.logout(); } catch { client.close(); }
    }
  };
  return http.createServer(async (req, res) => {
    if (req.method !== 'POST' || !['/folders', '/messages', '/message'].includes(req.url)) {
      res.writeHead(404).end();
      return;
    }
    const chunks = [];
    let length = 0;
    for await (const chunk of req) {
      length += chunk.length;
      if (length > 4096) { res.writeHead(413).end(); return; }
      chunks.push(chunk);
    }
    const raw = Buffer.concat(chunks);
    let arg;
    try { arg = JSON.parse(raw); } catch { res.writeHead(400).end(); return; }
    if (!arg || typeof arg !== 'object' || Array.isArray(arg)) { res.writeHead(400).end(); return; }
    try {
      let result;
      if (req.url === '/folders') {
        result = await withClient(async client => {
          const folders = await client.list();
          if (folders.length > 200) throw new Error('too many mail folders');
          return {folders: folders.filter(entry => !entry.flags?.has('\\NonExistent')).map(entry => ({
            path: entry.path, specialUse: entry.specialUse || '',
          }))};
        });
      } else if (req.url === '/message') {
        const folder = arg.folder || 'INBOX';
        if (typeof folder !== 'string' || !folder || folder.length > 255 || !Number.isInteger(arg.uid) || arg.uid < 1) {
          res.writeHead(400).end(); return;
        }
        result = await withClient(async client => {
          // EXAMINE keeps the message unread.
          const opened = await client.mailboxOpen(folder, {readOnly: true});
          const message = await client.fetchOne(String(arg.uid), {uid: true, envelope: true, internalDate: true, bodyStructure: true}, {uid: true});
          if (!message) return {folder, uid: arg.uid, found: false};
          const parts = messageParts(message.bodyStructure);
          const part = parts.plain || parts.html;
          let body = {text: '', truncated: false};
          if (part) {
            const {content} = await client.download(String(arg.uid), part.part || '1', {uid: true});
            body = await boundedText(content);
            if (part === parts.html) body.text = htmlText(body.text);
          }
          const addresses = list => (list || []).map(item => item.name ? `${item.name} <${item.address}>` : item.address).filter(Boolean).join(', ').slice(0, 2048);
          return {folder, uidValidity: Number(opened.uidValidity), uid: message.uid, found: true,
            from: addresses(message.envelope?.from), to: addresses(message.envelope?.to), cc: addresses(message.envelope?.cc),
            subject: (message.envelope?.subject || '').slice(0, 998), messageId: (message.envelope?.messageId || '').slice(0, 990),
            inReplyTo: (message.envelope?.inReplyTo || '').slice(0, 990),
            date: message.internalDate?.toISOString() || message.envelope?.date?.toISOString() || '',
            text: body.text, truncated: body.truncated, attachments: parts.attachments};
        });
      } else {
        const folder = arg.folder || 'INBOX';
        const limit = arg.limit || 25;
        const before = arg.beforeSequence;
        if (typeof folder !== 'string' || !folder || folder.length > 255 ||
            !Number.isInteger(limit) || limit < 1 || limit > 50 ||
            before !== undefined && (!Number.isInteger(before) || before < 1)) {
          res.writeHead(400).end(); return;
        }
        result = await withClient(async client => {
          const opened = await client.mailboxOpen(folder, {readOnly: true});
          const end = Math.min(opened.exists, before ? before - 1 : opened.exists);
          const start = Math.max(1, end - limit + 1);
          const rows = end < 1 ? [] : await client.fetchAll(`${start}:${end}`,
            {uid: true, envelope: true, internalDate: true});
          return {folder, uidValidity: Number(opened.uidValidity),
            messages: rows.reverse().map(row => ({uid: row.uid,
              from: (row.envelope?.from || []).map(item => item.address).filter(Boolean).join(', ').slice(0, 512),
              subject: (row.envelope?.subject || '').slice(0, 512),
              messageId: (row.envelope?.messageId || '').slice(0, 990),
              date: row.internalDate?.toISOString() || row.envelope?.date?.toISOString() || ''})),
            nextBeforeSequence: start > 1 ? start : null};
        });
      }
      res.writeHead(200, {'content-type': 'application/json'}).end(JSON.stringify(result));
    } catch (error) {
      console.error(JSON.stringify({event: 'mail_list_failed', code: error.code || 'UNKNOWN'}));
      res.writeHead(503).end();
    }
  });
}
