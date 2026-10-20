import http from 'node:http';
import nodemailer from 'nodemailer';

const host = process.env.SMTP_HOST;
const port = Number(process.env.SMTP_PORT || 465);
if (!host || !process.env.SMTP_USERNAME || !process.env.SMTP_PASSWORD) {
  throw new Error('SMTP sender configuration incomplete');
}

const tlsMode = process.env.SMTP_TLS || 'auto';
const secure = tlsMode === 'implicit' || (tlsMode === 'auto' && port === 465);
const transport = nodemailer.createTransport({
  host,
  port,
  secure,
  requireTLS: tlsMode === 'starttls' || (tlsMode === 'auto' && !secure),
  ignoreTLS: tlsMode === 'disabled',
  auth: {user: process.env.SMTP_USERNAME, pass: process.env.SMTP_PASSWORD},
  connectionTimeout: 15000,
  greetingTimeout: 15000,
  socketTimeout: 25000,
});
const composer = nodemailer.createTransport({streamTransport: true, buffer: true});
const saveSent = process.env.SMTP_SAVE_SENT === 'true';
const archiveURL = process.env.IMAP_ARCHIVE_URL || 'http://127.0.0.1:8081';

async function archiveRequest(path, body) {
  const response = await fetch(archiveURL + path, {method: body ? 'POST' : 'GET', body,
    signal: AbortSignal.timeout(35000)});
  if (!response.ok) throw new Error(`Sent archive HTTP ${response.status}`);
  return response.json();
}

const senderAddress = process.env.SMTP_FROM_ADDRESS;
const sender = process.env.SMTP_FROM_NAME
  ? {name: process.env.SMTP_FROM_NAME, address: senderAddress}
  : senderAddress;

// Only the Pod's mcp container reaches this loopback listener; it
// authorizes the worker before forwarding.
const server = http.createServer(async (req, res) => {
  if (req.method !== 'POST' || req.url !== '/send') {
    res.writeHead(404).end();
    return;
  }
  const chunks = [];
  let size = 0;
  for await (const chunk of req) {
    size += chunk.length;
    if (size > 128 * 1024) {
      res.writeHead(413).end();
      return;
    }
    chunks.push(chunk);
  }
  const body = Buffer.concat(chunks);
  let message;
  try {
    message = JSON.parse(body);
  } catch {
    res.writeHead(400).end();
    return;
  }
  if (!Array.isArray(message.to) || message.to.length < 1 || message.to.length > 20 ||
      typeof message.subject !== 'string' || typeof message.text !== 'string' ||
      message.subject.length > 998 || !message.text || message.text.length > 65536) {
    res.writeHead(400).end();
    return;
  }
  try {
    if (saveSent) {
      try { await archiveRequest('/sent/ready'); }
      catch {
        res.writeHead(503).end();
        return;
      }
    }
    const composed = await composer.sendMail({
      from: sender,
      replyTo: process.env.SMTP_REPLY_TO || senderAddress,
      to: message.to,
      subject: message.subject,
      text: message.text,
    });
    const result = await transport.sendMail({raw: composed.message, envelope: composed.envelope});
    if (!result.accepted?.length) {
      res.writeHead(502).end();
      return;
    }
    const sentCopy = {status: saveSent ? 'failed' : 'disabled'};
    if (saveSent) {
      try {
        const archived = await archiveRequest('/sent/append', composed.message);
        sentCopy.status = 'archived';
        sentCopy.folder = archived.folder;
      } catch {
        console.error(JSON.stringify({event: 'sent_copy_failed', messageId: composed.messageId}));
      }
    }
    res.writeHead(200, {'content-type': 'application/json'}).end(JSON.stringify({
      messageId: composed.messageId, accepted: result.accepted, rejected: result.rejected || [], sentCopy,
    }));
  } catch {
    res.writeHead(502).end();
  }
});

server.listen(Number(process.env.SMTP_API_PORT || 8083), '127.0.0.1');
