import {createHash} from 'node:crypto';
import {readFile} from 'node:fs/promises';
import {ImapFlow} from 'imapflow';
import amqp from 'amqplib';
import {createSentArchiveServer} from './sent-archive.js';
import {createMailListServer} from './list.js';

const env = process.env;
const required = name => {
  if (!env[name]) throw new Error(`${name} is required`);
  return env[name];
};
const namespace = required('AGENT_NAMESPACE');
const mailbox = required('MAILBOX_NAME');
const mailboxUID = required('MAILBOX_UID');
const folder = env.IMAP_FOLDER || 'INBOX';
const exchange = env.AMQP_EXCHANGE || 'agent.webhook.events';
// RabbitMQ checks user_id against the login, so the operator can trust it to
// identify this namespace's Mailbox ServiceAccount.
const userId = required('AMQP_USER_ID');

export function parseHeaders(raw) {
  if (!Buffer.isBuffer(raw) || raw.length > 60 * 1024) return {};
  const headers = {};
  let current;
  for (const line of raw.toString('latin1').split(/\r?\n/)) {
    if (line === '') break;
    if (/^[ \t]/.test(line)) {
      if (current) {
        headers[current][headers[current].length - 1] += ` ${line.trim()}`;
        if (headers[current].at(-1).length > 4096) return {};
      }
      continue;
    }
    const colon = line.indexOf(':');
    if (colon < 1) { current = undefined; continue; }
    const name = line.slice(0, colon).toLowerCase();
    if (!/^[a-z0-9-]{1,80}$/.test(name)) { current = undefined; continue; }
    current = name;
    (headers[name] ||= []).push(line.slice(colon + 1).trim());
    if (Object.keys(headers).length > 128 || headers[name].length > 4 || headers[name].at(-1).length > 4096) return {};
  }
  return headers;
}

// Logs in with the projected publisher token and refreshes it on the open
// connection before kubelet's rotated token expires.
async function brokerConnect() {
  const tokenFile = required('AGENT_RABBITMQ_TOKEN_FILE');
  const token = async () => (await readFile(tokenFile, 'utf8')).trim();
  const url = new URL(required('AMQP_URL'));
  url.username = '';
  url.password = await token();
  const connection = await amqp.connect(url.href);
  const timer = setInterval(async () => {
    try { await connection.updateSecret(Buffer.from(await token()), 'token refresh'); }
    catch (error) { console.error(JSON.stringify({event: 'amqp_token_refresh_failed', code: error.code || 'UNKNOWN'})); }
  }, 300000);
  timer.unref();
  connection.on('close', () => clearInterval(timer));
  return connection;
}

export function envelopeFor(uidValidity, uid, headers, checkpoint = false, threadId = '') {
  const event = {version: 1, namespace, mailbox, mailboxUID, folder,
    uidValidity: Number(uidValidity), uid: Number(uid), headers, checkpoint,
    threadId: typeof threadId === 'string' && threadId.length <= 256 ? threadId : ''};
  const messageId = createHash('sha256').update(`${mailboxUID}\0${folder}\0${uidValidity}\0${uid}`).digest('hex');
  const routingKey = `mailbox.${namespace}.${mailbox}`;
  const body = Buffer.from(JSON.stringify(event));
  return {messageId, routingKey, body, headers: {
    'x-namespace': namespace, 'x-mailbox': mailbox, 'x-mailbox-uid': mailboxUID, 'x-source': 'imap'}};
}

async function publish(channel, uidValidity, uid, headers, checkpoint = false, threadId = '') {
  let delivery = envelopeFor(uidValidity, uid, headers, checkpoint, threadId);
  if (delivery.body.length > 64 * 1024) delivery = envelopeFor(uidValidity, uid, {}, checkpoint);
  let returned = false;
  const onReturn = message => { if (message.properties.messageId === delivery.messageId) returned = true; };
  channel.on('return', onReturn);
  try {
    channel.publish(exchange, delivery.routingKey, delivery.body, {persistent: true, mandatory: true,
      contentType: 'application/json', messageId: delivery.messageId, userId, headers: delivery.headers});
    await channel.waitForConfirms();
    if (returned) throw new Error('mail event was not routed to the operator queue');
  } finally {
    channel.off('return', onReturn);
  }
}

async function run() {
  const port = Number(env.IMAP_PORT || 993);
  const tls = env.IMAP_TLS || 'auto';
  const useTLS = tls !== 'disabled';
  const secure = tls === 'implicit' || tls === 'auto' && port === 993;
  const client = new ImapFlow({host: required('IMAP_HOST'), port, secure,
    doSTARTTLS: useTLS && !secure, auth: {user: required('IMAP_USERNAME'), pass: required('IMAP_PASSWORD')},
    logger: false, maxIdleTime: 240000, connectionTimeout: 15000, socketTimeout: 300000});
  const connection = await brokerConnect();
  const channel = await connection.createConfirmChannel();
  let lastUID = Number(env.MAIL_CURSOR_LAST_UID || 0);
  let previousValidity = Number(env.MAIL_CURSOR_UIDVALIDITY || 0);
  let draining = false;
  let again = false;
  client.on('error', error => console.error(JSON.stringify({event: 'imap_error', code: error.code || 'UNKNOWN'})));
  const drain = async () => {
    if (draining) { again = true; return; }
    draining = true;
    try {
      do {
        again = false;
        const lock = await client.getMailboxLock(folder, {readOnly: true});
        try {
          const status = await client.status(folder, {uidNext: true, uidValidity: true});
          const validity = Number(status.uidValidity);
          if (!Number.isSafeInteger(validity) || validity < 1 || !Number.isSafeInteger(status.uidNext)) throw new Error('invalid IMAP status');
          if (previousValidity === 0) {
            lastUID = status.uidNext - 1;
            await publish(channel, validity, lastUID, {}, true);
          }
          else if (validity !== previousValidity) lastUID = 0;
          previousValidity = validity;
          while (lastUID < status.uidNext - 1) {
            const end = Math.min(lastUID + 100, status.uidNext - 1);
            const messages = await client.fetchAll(`${lastUID + 1}:${end}`, {uid: true, headers: true, threadId: true}, {uid: true});
            for (const item of messages.sort((a, b) => a.uid - b.uid)) {
              await publish(channel, validity, item.uid, parseHeaders(item.headers), false, item.threadId);
              lastUID = item.uid;
            }
            // UIDs can have gaps after deletion. A confirmed scan may advance
            // past them; the operator cursor still advances per delivered UID.
            lastUID = end;
          }
        } finally { lock.release(); }
      } while (again);
    } finally { draining = false; }
  };
  try {
    await client.connect();
    await client.mailboxOpen(folder, {readOnly: true});
    client.on('exists', event => {
      if (event.path === folder) void drain().catch(error => { console.error(JSON.stringify({event: 'imap_drain_failed', code: error.code || 'UNKNOWN'})); client.close(); });
    });
    await drain();
    console.log(JSON.stringify({event: 'imap_connected', mailbox, folder}));
    await new Promise((resolve, reject) => {
      client.once('close', resolve);
      connection.once('close', () => reject(new Error('AMQP disconnected')));
    });
  } finally {
    try { client.close(); } catch { /* connection may already be closed */ }
    await connection.close().catch(() => {});
  }
}

if (process.argv[1]?.endsWith('/mailbox/imap.js')) {
  const listing = createMailListServer({host: required('IMAP_HOST'), port: Number(env.IMAP_PORT || 993),
    tls: env.IMAP_TLS || 'auto', username: required('IMAP_USERNAME'), password: required('IMAP_PASSWORD')});
  listing.listen(8082, '127.0.0.1');
  if (env.IMAP_ARCHIVE_ENABLED === 'true') {
    const archive = createSentArchiveServer({host: required('IMAP_HOST'), port: Number(env.IMAP_PORT || 993),
      tls: env.IMAP_TLS || 'auto', username: required('IMAP_USERNAME'), password: required('IMAP_PASSWORD'),
      folder: env.IMAP_SENT_FOLDER || ''});
    archive.listen(8081, '127.0.0.1');
  }
  for (;;) {
    try { await run(); } catch (error) { console.error(JSON.stringify({event: 'mailbox_retry', code: error.code || 'UNKNOWN'})); }
    await new Promise(resolve => setTimeout(resolve, 5000));
  }
}
