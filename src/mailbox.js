import {ImapFlow} from 'imapflow';
import nodemailer from 'nodemailer';
import MailComposer from 'nodemailer/lib/mail-composer';
import {simpleParser} from 'mailparser';
import {githubNotificationReference} from './github-mail.js';
import {checkConnection, unconfiguredConnection} from './connectivity.js';
import {Problem, text, digest, activity, prompt, controllerActor, terminal} from './domain.js';
import {agentIdentity} from './identity.js';
import {taskCommunication} from './agent-mcp.js';

const address = value => typeof value === 'string' && /^[^\s<>@,;]+@[^\s<>@,;]+\.[^\s<>@,;]+$/.test(value) && value.length <= 254;
const messageId = value => typeof value === 'string' && /^<[^<>\s]{1,990}>$/.test(value);
const bool = (value, fallback = true) => value === undefined ? fallback : /^(true|1|yes)$/i.test(value);
const safeError = error => /^[A-Z0-9_]{1,30}$/.test(error.code || '') ? error.code : error.authenticationFailed ? 'AUTHENTICATION_FAILED' : 'CONNECTION_FAILED';

export function mailConfiguration(env = process.env) {
  const imapPort = Number(env.IMAP_PORT || 993), smtpPort = Number(env.SMTP_PORT || 465);
  const imapTLS = bool(env.IMAP_TLS), smtpTLS = bool(env.SMTP_TLS);
  const identity = agentIdentity(env), from = env.SMTP_FROM || identity.email;
  return {folder: env.IMAP_MAILBOX || 'INBOX', saveSent: bool(env.SMTP_SAVE_SENT, false), sentMailbox: env.IMAP_SENT_MAILBOX,
    imap: env.IMAP_HOST ? {host: env.IMAP_HOST, port: imapPort, secure: imapTLS && imapPort === 993,
      ...(!imapTLS || imapPort !== 993 ? {doSTARTTLS: imapTLS} : {}),
      auth: {user: env.IMAP_USERNAME || identity.username, pass: env.IMAP_PASSWORD},
      logger: false, maxIdleTime: 240000, connectionTimeout: 15000, socketTimeout: 300000} : null,
    smtp: env.SMTP_HOST ? {host: env.SMTP_HOST, port: smtpPort, secure: smtpTLS && smtpPort === 465,
      requireTLS: smtpTLS && smtpPort !== 465, ignoreTLS: !smtpTLS,
      auth: {user: env.SMTP_USERNAME || identity.username, pass: env.SMTP_PASSWORD},
      connectionTimeout: 15000, greetingTimeout: 15000, socketTimeout: 30000, logger: false, debug: false,
      disableFileAccess: true, disableUrlAccess: true} : null,
    from, replyTo: env.SMTP_REPLY_TO || from,
    allowedSenders: (env.IMAP_ALLOWED_SENDERS || '').split(',').map(value => value.trim().toLowerCase()).filter(Boolean)};
}

export function replyReferences(mail) {
  const references = Array.isArray(mail.references) ? mail.references : (mail.references || '').match(/<[^<>\s]+>/g) || [];
  return [...new Set([mail.inReplyTo, ...references.slice().reverse()].filter(messageId))];
}

const queueScript = `local previous=redis.call('GET',KEYS[1]); if previous then return previous end;
redis.call('SET',KEYS[1],ARGV[1]); redis.call('SADD',KEYS[2],ARGV[2]);
redis.call('SET',KEYS[3],ARGV[3]); redis.call('SADD',KEYS[4],KEYS[1],KEYS[3]); return ARGV[1]`;
const cursorScript = `local previous=redis.call('GET',KEYS[1]); local next=cjson.decode(ARGV[1]);
if previous then local p=cjson.decode(previous); if p.uidValidity==next.uidValidity and p.lastUid>next.lastUid then return previous end end;
redis.call('SET',KEYS[1],ARGV[1]); return ARGV[1]`;

export class Mailbox {
  constructor(store, settings = mailConfiguration(), dependencies = {}) {
    this.store = store; this.redis = store.redis; this.settings = settings;
    this.imapFactory = dependencies.imapFactory || (options => new ImapFlow(options));
    this.imapConfigured = Boolean(settings.imap?.auth?.user && settings.imap.auth.pass);
    const smtpConfigured = settings.smtp?.auth?.user && settings.smtp.auth.pass && address(settings.from);
    this.smtpFactory = dependencies.smtpFactory || (options => nodemailer.createTransport(options));
    this.transport = dependencies.transport || (smtpConfigured ? this.smtpFactory(settings.smtp) : null);
    this.parse = dependencies.parse || simpleParser;
    this.onGithubNotification = dependencies.onGithubNotification;
    this.onException = dependencies.onException || (() => {});
    this.log = dependencies.log || (event => console.log(JSON.stringify({component: 'mailbox', ...event})));
    this.prefix = store.prefix + 'mail:'; this.state = {
      imap: settings.imap ? this.imapConfigured ? 'disconnected' : 'incomplete_configuration' : 'disabled',
      smtp: settings.smtp ? this.transport ? 'configured' : 'incomplete_configuration' : 'disabled'};
    this.lastLeader = 0; this.retryAt = 0; this.checks = {};
    this.timer = setInterval(() => { if (this.client && Date.now() - this.lastLeader > 45000) this.client.close(); }, 5000);
    this.timer.unref();
  }
  async probeInbound() {
    if (!this.imapConfigured) return unconfiguredConnection('inbound-mail', Boolean(this.settings.imap));
    let client;
    const result = await checkConnection('inbound-mail', 'IMAP credentials accepted and inbox opened read-only', async () => {
      client = this.imapFactory({...this.settings.imap, disableAutoIdle: true, connectionTimeout: 10000,
        greetingTimeout: 10000, socketTimeout: 15000});
      client.on('error', () => {});
      await client.connect();
      await client.mailboxOpen(this.settings.folder, {readOnly: true});
    }, {onTimeout: () => client?.close()});
    try { client?.close(); } catch { /* Cleanup cannot invalidate an authenticated check. */ }
    return result;
  }
  async probeOutbound() {
    if (!this.transport) return unconfiguredConnection('outbound-mail', Boolean(this.settings.smtp));
    let transport;
    const result = await checkConnection('outbound-mail', 'SMTP connection and authentication verified; no email sent', async () => {
      transport = this.smtpFactory({...this.settings.smtp, connectionTimeout: 10000, greetingTimeout: 10000, socketTimeout: 15000});
      await transport.verify();
    }, {onTimeout: () => transport?.close()});
    try { transport?.close(); } catch { /* Cleanup cannot invalidate an authenticated check. */ }
    return result;
  }
  bindingKey(id) { return this.prefix + 'message:' + digest(id); }
  checked(name, available, detail) {
    this.checks[name] = {name, status: available ? 'available' : 'unavailable', detail,
      checkedAt: new Date().toISOString(), source: 'operation'};
  }
  context() {
    return {owner: 'codemowers/agent-controller', scope: 'current_task_threads',
      imap: {configured: this.imapConfigured, state: this.state.imap},
      smtp: {configured: Boolean(this.transport), state: this.state.smtp, from: this.settings.from},
      saveSent: Boolean(this.settings.saveSent), sentMailbox: this.settings.sentMailbox || this.sentMailbox,
      replyRouting: 'Replies to emails sent by this Task wake this same Task. Unmatched inbox mail is not exposed to workers.'};
  }
  async taskMessages(taskId) {
    const task = await this.store.get(taskId);
    return {...this.context(), communication: taskCommunication(task), messages: task.events.filter(event => event.source === 'email'
      && (event.kind === 'email' || event.kind === 'event')).slice(-30).map(event => ({
      direction: event.direction || 'inbound', messageId: event.messageId, inReplyTo: event.inReplyTo,
      from: event.direction === 'outbound' ? this.settings.from : event.actor.subject,
      createdAt: event.createdAt, content: event.content, deliveryState: event.deliveryState}))};
  }
  async taskStatus(taskId) {
    const task = await this.store.get(taskId);
    const keys = (await this.redis.sMembers(this.prefix + 'task:' + taskId)).filter(key => key.startsWith(this.prefix + 'out:'));
    const entries = keys.length ? (await this.redis.mGet(keys)).filter(Boolean).map(JSON.parse) : [];
    return {...this.context(), communication: taskCommunication(task), configured: Boolean(this.transport),
      emails: entries.map(({id, messageId, state, createdAt, sentAt, errorCode, sentCopyState, sentCopyAt, sentCopyErrorCode}) =>
        ({id, messageId, state, createdAt, sentAt, errorCode, sentCopyState, sentCopyAt, sentCopyErrorCode}))};
  }
  async removeTask(taskId) {
    const index = this.prefix + 'task:' + taskId, keys = await this.redis.sMembers(index);
    const outbox = keys.filter(key => key.startsWith(this.prefix + 'out:'));
    if (outbox.length) await this.redis.sRem(this.prefix + 'outbox', outbox.map(key => key.slice((this.prefix + 'out:').length)));
    await this.redis.del([...keys, ...outbox.flatMap(key => [key + ':lock', key + ':sent-copy-lock']), index]);
  }
  async status() {
    const ids = await this.redis.sMembers(this.prefix + 'outbox');
    const entries = ids.length ? (await this.redis.mGet(ids.map(id => this.prefix + 'out:' + id))).filter(Boolean).map(JSON.parse) : [];
    return {imap: {configured: this.imapConfigured, state: this.state.imap, lastReceivedAt: this.lastReceivedAt},
      smtp: {configured: Boolean(this.transport), state: this.state.smtp, from: this.settings.from,
        saveSent: Boolean(this.settings.saveSent), sentMailbox: this.settings.sentMailbox || this.sentMailbox,
        outbox: entries.sort((a, b) => b.createdAt.localeCompare(a.createdAt)).slice(0, 10)
          .map(({id, taskId, state, createdAt, sentAt, errorCode, sentCopyState}) => ({id, taskId, state, createdAt, sentAt, errorCode, sentCopyState}))}};
  }
  async queue(task, actor, args) {
    if (!this.transport || !address(this.settings.from) || !address(this.settings.replyTo)) throw new Problem(503, 'SMTP sending is not configured');
    if (task.deleteRequested || task.completionRequested || terminal.has(task.phase) || task.desiredState === 'Cancelled') throw new Problem(409, 'Task no longer accepts email');
    const to = typeof args.to === 'string' ? [args.to] : args.to;
    if (!Array.isArray(to) || !to.length || to.length > 10 || !to.every(address)) throw new Problem(400, 'Expected one to ten email addresses');
    const subject = text(args.subject, 200);
    if (/[\r\n]/.test(subject)) throw new Problem(400, 'Email subject must be one line');
    const body = text(args.text, 16000), request = text(args.idempotencyKey, 200);
    const id = digest(task.id + ':' + request), key = this.prefix + 'out:' + id;
    const mid = `<agent-${id}@${this.settings.from.split('@')[1]}>`;
    let inReplyTo;
    if (args.replyToMessageId) {
      if (!messageId(args.replyToMessageId)) throw new Problem(400, 'Invalid reply Message-ID');
      const binding = await this.redis.get(this.bindingKey(args.replyToMessageId));
      if (!binding || JSON.parse(binding).taskId !== task.id) throw new Problem(400, 'Reply Message-ID does not belong to this Task');
      inReplyTo = args.replyToMessageId;
    }
    const contentDigest = digest(JSON.stringify({to, subject, body, inReplyTo}));
    const entry = {id, taskId: task.id, messageId: mid, to, subject, text: body, inReplyTo,
      references: inReplyTo ? [inReplyTo] : [], contentDigest, state: 'queued', createdAt: new Date().toISOString()};
    const value = JSON.parse(await this.redis.eval(queueScript, {keys: [key, this.prefix + 'outbox', this.bindingKey(mid), this.prefix + 'task:' + task.id],
      arguments: [JSON.stringify(entry), id, JSON.stringify({taskId: task.id, participants: to.map(value => value.toLowerCase())})]}));
    if (value.contentDigest !== contentDigest) throw new Problem(409, 'Email idempotency key already used with different content');
    await this.store.mutate(task.id, 'mail-queued:' + id, current => {
      current.emailConversation = true;
      activity(current, actor, `Email to ${to.join(', ')}\nSubject: ${subject}\n\n${body}`, 'email',
        {kind: 'email', direction: 'outbound', mailId: id, messageId: mid, deliveryState: value.state});
    });
    return {id, messageId: mid, state: value.state};
  }
  tick() {
    this.lastLeader = Date.now();
    if (this.imapConfigured && !this.client && !this.connecting && Date.now() >= this.retryAt) {
      this.connecting = this.connect().catch(error => this.scanFailed(error, this.client, 'imap_connect'))
        .finally(() => { this.connecting = null; });
    }
    if (this.client && this.state.imap === 'connected' && Date.now() >= (this.nextPoll || 0)) {
      this.nextPoll = Date.now() + 30000;
      void this.scheduleDrain(this.client);
    }
    if (this.transport && !this.sending) this.sending = this.flush().catch(error => {
      this.log({event: 'smtp_flush_failed', code: safeError(error)});
      void this.onException(error, 'smtp_flush');
    }).finally(() => { this.sending = null; });
  }
  async connect() {
    const client = this.imapFactory(this.settings.imap); this.client = client; this.state.imap = 'connecting';
    client.on('error', error => this.scanFailed(error, client, 'imap_connection'));
    client.on('close', () => {
      if (this.client !== client) return;
      this.client = null; this.state.imap = 'disconnected'; this.retryAt = Date.now() + 10000;
      this.checked('inbound-mail', false, 'IMAP connection closed; reconnect scheduled');
    });
    client.on('exists', event => { if (event.path === this.settings.folder) void this.scheduleDrain(client); });
    try {
      await client.connect(); await client.mailboxOpen(this.settings.folder, {readOnly: true});
    } catch (error) {
      this.log({event: 'imap_connection_failed', code: safeError(error)}); client.close();
      if (this.client === client) this.client = null;
      this.state.imap = 'failed'; this.retryAt = Date.now() + 30000;
      this.checked('inbound-mail', false, 'IMAP authentication or inbox access failed (' + safeError(error) + ')');
      void this.onException(error, 'imap_connect'); return;
    }
    this.state.imap = 'connected'; this.nextPoll = Date.now() + 30000; this.log({event: 'imap_connected'});
    this.checked('inbound-mail', true, 'IMAP login and inbox access succeeded');
    await this.scheduleDrain(client);
  }
  scheduleDrain(client) {
    if (this.draining) { this.drainAgain = true; return this.draining; }
    this.draining = this.drain(client).catch(error => this.scanFailed(error, client, 'imap_poll')).finally(() => {
      this.draining = null;
      if (this.drainAgain && this.client === client) { this.drainAgain = false; void this.scheduleDrain(client); }
    });
    return this.draining;
  }
  scanFailed(error, client, kind) {
    this.checked('inbound-mail', false, 'IMAP operation failed (' + safeError(error) + ')');
    this.log({event: 'imap_scan_failed', code: safeError(error), operation: error.agentImapOperation,
      responseStatus: /^(NO|BAD)$/.test(error.responseStatus) ? error.responseStatus : undefined});
    if (this.client === client) {
      this.client = null; client?.close();
      this.state.imap = 'failed'; this.retryAt = Date.now() + 30000;
    }
    void this.onException(error, kind);
  }
  async drain(client) {
    if (this.client !== client || !client.mailbox || Date.now() - this.lastLeader > 45000) return;
    let lock, operation = {command: 'EXAMINE'};
    try {
      lock = await client.getMailboxLock(this.settings.folder, {readOnly: true});
      // EXISTS notifications carry a message count, not UIDNEXT. The selected
      // mailbox's cached UIDNEXT can stay unchanged while new mail arrives.
      operation = {command: 'STATUS'};
      const status = await client.status(this.settings.folder, {uidNext: true, uidValidity: true});
      if (!status || !Number.isSafeInteger(status.uidNext) || status.uidNext < 1 || status.uidValidity === undefined)
        throw Object.assign(new Error('IMAP status unavailable'), {code: 'IMAP_STATUS_FAILED'});
      const uidNext = status.uidNext, uidValidity = String(status.uidValidity), cursorKey = this.prefix + 'cursor';
      this.lastCheckedAt = new Date().toISOString();
      this.checked('inbound-mail', true, 'IMAP inbox status check succeeded');
      if (uidNext > 1 && this.onGithubNotification && !await this.redis.exists(this.prefix + 'github-routing-v1')) {
        const recent = [];
        operation = {command: 'UID FETCH HEADERS', fromUid: Math.max(1, uidNext - 200), throughUid: uidNext - 1};
        for await (const item of client.fetch(`${Math.max(1, uidNext - 200)}:${uidNext - 1}`, {
          uid: true, headers: ['from', 'message-id', 'in-reply-to', 'references']}, {uid: true})) recent.push(item);
        for (const item of recent) {
          if (!item.headers) continue;
          const mail = await this.parse(item.headers, {skipTextToHtml: true});
          const from = mail.from?.value?.[0]?.address?.toLowerCase();
          if (['notifications@github.com', 'noreply@github.com'].includes(from) && githubNotificationReference(mail))
            await this.githubNotification(mail, `${uidValidity}:${item.uid}`, {backfill: true});
        }
        await this.redis.set(this.prefix + 'github-routing-v1', '1');
      }
      const raw = await this.redis.get(cursorKey), saved = raw && JSON.parse(raw);
      // First activation watches new arrivals. UIDVALIDITY changes restart UID
      // scanning, while Message-ID/task receipts deduplicate previously seen mail.
      let lastUid = saved ? saved.uidValidity === uidValidity ? saved.lastUid : 0 : uidNext - 1;
      const previousUid = lastUid;
      while (lastUid < uidNext - 1 && this.client === client && Date.now() - this.lastLeader <= 45000) {
        const end = Math.min(lastUid + 100, uidNext - 1), messages = [];
        operation = {command: 'UID FETCH SIZE', fromUid: lastUid + 1, throughUid: end};
        for await (const item of client.fetch(`${lastUid + 1}:${end}`, {uid: true, size: true}, {uid: true})) messages.push(item);
        for (const item of messages.sort((a, b) => a.uid - b.uid)) {
          if (item.size > 256000) { this.log({event: 'imap_message_ignored', reason: 'too_large', uid: item.uid}); continue; }
          operation = {command: 'UID FETCH SOURCE', uid: item.uid};
          const message = await client.fetchOne(item.uid, {source: true}, {uid: true});
          if (message?.source) {
            const parsed = await this.parse(message.source, {skipHtmlToText: false, skipTextToHtml: true});
            await this.receive(parsed, `${uidValidity}:${item.uid}`);
          }
        }
        lastUid = end;
        await this.redis.eval(cursorScript, {keys: [cursorKey], arguments: [JSON.stringify({uidValidity, lastUid})]});
      }
      if (!saved) await this.redis.set(cursorKey, JSON.stringify({uidValidity, lastUid}));
      if (lastUid > previousUid) this.log({event: 'imap_scan_complete', fromUid: previousUid + 1, throughUid: lastUid});
    } catch (error) { error.agentImapOperation = operation; throw error; }
    finally { lock?.release(); }
  }
  async receive(mail, delivery) {
    const from = mail.from?.value?.[0]?.address?.toLowerCase();
    if (!address(from) || from === this.settings.from?.toLowerCase()) return;
    const ids = replyReferences(mail), candidates = [];
    for (const reference of ids) {
      const raw = await this.redis.get(this.bindingKey(reference));
      if (raw) candidates.push(JSON.parse(raw));
    }
    const taskIds = [...new Set(candidates.map(value => value.taskId))];
    if (taskIds.length > 1) { this.log({event: 'imap_message_ignored', reason: 'ambiguous_thread'}); return; }
    if (!taskIds.length) {
      if (['notifications@github.com', 'noreply@github.com'].includes(from) && this.onGithubNotification) {
        await this.githubNotification(mail, delivery); return;
      }
      this.log({event: 'imap_message_ignored', reason: 'unmatched_thread'}); return;
    }
    if (!candidates.some(value => value.participants.includes(from)) && !this.settings.allowedSenders.includes(from)) {
      this.log({event: 'imap_message_ignored', reason: 'not_thread_participant'}); return;
    }
    try { await this.store.get(taskIds[0]); }
    catch (error) { if (error.status === 404) { this.log({event: 'imap_message_ignored', reason: 'task_deleted'}); return; } throw error; }
    const auto = mail.headers?.get('auto-submitted');
    if (auto && auto !== 'no') { this.log({event: 'imap_message_ignored', reason: 'automated_reply'}); return; }
    const taskId = taskIds[0], mid = messageId(mail.messageId) ? mail.messageId : `<imap-${digest(delivery)}@local>`;
    const body = (mail.text || '').trim().slice(0, 15500);
    if (!body) return;
    const subject = String(mail.subject || 'Email reply').replace(/[\r\n]/g, ' ').slice(0, 200);
    const actor = {type: 'external', provider: 'email', subject: from, name: from};
    let accepted = false;
    await this.store.mutate(taskId, 'mail-inbound:' + digest(mid), current => {
      if (current.deleteRequested || current.completionRequested || terminal.has(current.phase) || current.desiredState === 'Cancelled') return;
      current.emailConversation = true;
      const source = activity(current, actor, `Subject: ${subject}\n\n${body}`, 'email',
        {kind: 'event', messageId: mid, inReplyTo: mail.inReplyTo});
      prompt(current, controllerActor, `Email reply from ${from} to this Task's email thread.\nSubject: ${subject}\nMessage-ID: ${mid}\n\n${body}`,
        'controller', {causedBy: [source.id]});
      accepted = true;
    });
    const key = this.bindingKey(mid);
    await this.redis.multi().set(key, JSON.stringify({taskId, participants: [...new Set(candidates.flatMap(value => value.participants))]}))
      .sAdd(this.prefix + 'task:' + taskId, key).exec();
    if (accepted) { this.lastReceivedAt = new Date().toISOString(); this.log({event: 'imap_task_wakeup', taskId}); }
  }
  async githubNotification(mail, delivery, options = {}) {
    try {
      const result = await this.onGithubNotification(mail, delivery, options);
      this.log({event: 'imap_github_notification', outcome: result.outcome, reason: result.reason,
        repository: result.repository, issue: result.issue, taskIds: result.taskIds || (result.taskId ? [result.taskId] : [])});
    } catch (error) {
      if ([400, 401, 403, 404, 410, 422].includes(error.status)) {
        this.log({event: 'imap_github_notification_ignored', reason: 'github_lookup_denied', status: error.status}); return;
      }
      throw error;
    }
  }
  async flush() {
    for (const id of await this.redis.sMembers(this.prefix + 'outbox')) {
      if (Date.now() - this.lastLeader > 45000) return;
      const key = this.prefix + 'out:' + id, entry = JSON.parse(await this.redis.get(key));
      if (entry?.state === 'sending' && !await this.redis.exists(key + ':lock')) {
        entry.state = 'uncertain'; await this.redis.set(key, JSON.stringify(entry));
        this.checked('outbound-mail', false, 'SMTP delivery outcome unknown; not automatically resent');
      }
      if (!entry) continue;
      if (['sent', 'partially_sent'].includes(entry.state)) { await this.archiveSent(entry); continue; }
      if (entry.state !== 'queued') continue;
      let task;
      try { task = await this.store.get(entry.taskId); }
      catch (error) { if (error.status !== 404) throw error; entry.state = 'cancelled'; await this.redis.set(key, JSON.stringify(entry)); continue; }
      if (task.deleteRequested || task.completionRequested || terminal.has(task.phase) || task.desiredState === 'Cancelled') {
        entry.state = 'cancelled'; await this.redis.set(key, JSON.stringify(entry)); continue;
      }
      if (!await this.redis.set(key + ':lock', 'sending', {NX: true, EX: 90})) continue;
      entry.state = 'sending'; await this.redis.set(key, JSON.stringify(entry));
      try {
        const result = await this.transport.sendMail(this.mailOptions(entry));
        entry.state = result.rejected?.length ? 'partially_sent' : 'sent'; entry.sentAt = new Date().toISOString();
      } catch (error) {
        // SMTP cannot promise exactly-once delivery after a lost connection.
        // Keep uncertainty visible instead of silently sending the same mail again.
        entry.state = error.responseCode >= 400 ? 'failed' : 'uncertain'; entry.errorCode = safeError(error);
      }
      await this.redis.set(key, JSON.stringify(entry));
      await this.store.mutate(entry.taskId, 'mail-delivery:' + id, current => {
        const event = current.events.find(value => value.mailId === id);
        if (event) { event.deliveryState = entry.state; event.sentAt = entry.sentAt; }
      });
      this.state.smtp = entry.state === 'sent' ? 'ready' : entry.state;
      this.checked('outbound-mail', entry.state === 'sent', entry.state === 'sent' ? 'SMTP accepted the last email'
        : 'SMTP delivery ' + entry.state + (entry.errorCode ? ' (' + entry.errorCode + ')' : ''));
      this.log({event: 'smtp_delivery', taskId: entry.taskId, mailId: id, state: entry.state});
      if (['sent', 'partially_sent'].includes(entry.state)) await this.archiveSent(entry);
    }
  }
  mailOptions(entry) {
    return {from: this.settings.from, replyTo: this.settings.replyTo, to: entry.to,
      subject: entry.subject, text: entry.text, messageId: entry.messageId, date: new Date(entry.createdAt),
      disableFileAccess: true, disableUrlAccess: true,
      ...(entry.inReplyTo ? {inReplyTo: entry.inReplyTo, references: entry.references} : {})};
  }
  async archiveSent(entry) {
    const client = this.client;
    if (!this.settings.saveSent || !client || this.state.imap !== 'connected' || entry.sentCopyState === 'saved'
      || Date.parse(entry.sentCopyRetryAt || '') > Date.now() || Date.now() - this.lastLeader > 45000) return;
    const key = this.prefix + 'out:' + entry.id, lockKey = key + ':sent-copy-lock';
    if (!await this.redis.set(lockKey, 'archiving', {NX: true, EX: 90})) return;
    try {
      let folder = this.settings.sentMailbox || this.sentMailbox;
      if (!folder) {
        const folders = (await client.list()).filter(value => value.specialUse === '\\Sent');
        if (folders.length !== 1) throw Object.assign(new Error('Configure IMAP_SENT_MAILBOX for this account'), {code: 'SENT_MAILBOX_NOT_UNIQUE'});
        folder = this.sentMailbox = folders[0].path;
      }
      const lock = await client.getMailboxLock(folder, {readOnly: true});
      try {
        const existing = await client.search({header: {'message-id': entry.messageId}}, {uid: true});
        if (!Array.isArray(existing)) throw Object.assign(new Error('Sent copy lookup failed'), {code: 'SENT_SEARCH_FAILED'});
        if (!existing.length) {
          const raw = await new MailComposer(this.mailOptions(entry)).compile().build();
          const appended = await client.append(folder, raw, ['\\Seen'], new Date(entry.sentAt));
          if (!appended) throw Object.assign(new Error('Sent copy append failed'), {code: 'SENT_APPEND_FAILED'});
        }
      } finally {
        try { if (this.client === client) await client.mailboxOpen(this.settings.folder, {readOnly: true}); }
        finally { lock.release(); }
      }
      entry.sentCopyState = 'saved'; entry.sentCopyAt = new Date().toISOString();
      delete entry.sentCopyErrorCode; delete entry.sentCopyRetryAt;
      this.log({event: 'smtp_sent_copy_saved', taskId: entry.taskId, mailId: entry.id});
    } catch (error) {
      entry.sentCopyState = 'retry_pending'; entry.sentCopyErrorCode = safeError(error);
      entry.sentCopyRetryAt = new Date(Date.now() + 30000).toISOString();
      this.log({event: 'smtp_sent_copy_failed', taskId: entry.taskId, mailId: entry.id, code: entry.sentCopyErrorCode});
      this.scanFailed(error, client, 'sent_archive');
    } finally {
      // Delivery state stays sent even if APPEND fails. Before retrying APPEND,
      // search the Message-ID so a lost acknowledgement cannot duplicate a copy.
      if (await this.redis.exists(key)) await this.redis.set(key, JSON.stringify(entry));
      await this.redis.del(lockKey);
    }
  }
  close() { clearInterval(this.timer); this.lastLeader = 0; this.client?.close(); this.transport?.close?.(); }
}
