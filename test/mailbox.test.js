import test from 'node:test';
import assert from 'node:assert/strict';
import {EventEmitter} from 'node:events';
import {Mailbox, mailConfiguration, replyReferences} from '../src/mailbox.js';
import {newTask, changeState, workerIdleTimeoutSeconds} from '../src/domain.js';
import {installFaultHandler} from '../src/faults.js';
import {simpleParser} from 'mailparser';

class MemoryRedis {
  values = new Map(); sets = new Map();
  async get(key) { return this.values.get(key) || null; }
  async set(key, value, options = {}) { if (options.NX && this.values.has(key)) return null; this.values.set(key, value); return 'OK'; }
  async exists(key) { return Number(this.values.has(key)); }
  async sAdd(key, items) { const set = this.sets.get(key) || new Set(); for (const value of Array.isArray(items) ? items : [items]) set.add(value); this.sets.set(key, set); }
  async sMembers(key) { return [...(this.sets.get(key) || [])]; }
  async sRem(key, items) { for (const value of Array.isArray(items) ? items : [items]) this.sets.get(key)?.delete(value); }
  async mGet(keys) { return Promise.all(keys.map(key => this.get(key))); }
  async del(keys) { for (const key of Array.isArray(keys) ? keys : [keys]) { this.values.delete(key); this.sets.delete(key); } }
  async eval(script, {keys, arguments: args}) {
    if (keys.length === 4) {
      const previous = await this.get(keys[0]); if (previous) return previous;
      await this.set(keys[0], args[0]); await this.sAdd(keys[1], args[1]); await this.set(keys[2], args[2]);
      await this.sAdd(keys[3], [keys[0], keys[2]]); return args[0];
    }
    const prior = JSON.parse(await this.get(keys[0]) || 'null'), next = JSON.parse(args[0]);
    if (prior?.uidValidity === next.uidValidity && prior.lastUid > next.lastUid) return JSON.stringify(prior);
    await this.set(keys[0], args[0]); return args[0];
  }
  multi() {
    const commands = [], transaction = {set: (...args) => { commands.push(() => this.set(...args)); return transaction; },
      sAdd: (...args) => { commands.push(() => this.sAdd(...args)); return transaction; },
      exec: async () => { for (const command of commands) await command(); }};
    return transaction;
  }
}
function fixture() {
  const redis = new MemoryRedis(), tasks = new Map(), receipts = new Set();
  const store = {redis, prefix: 'agent:test:mail:', get: async id => { const task = tasks.get(id); if (!task) throw Object.assign(new Error(), {status: 404}); return task; },
    mutate: async (id, key, fn) => { const task = await store.get(id); if (!receipts.has(id + key)) { fn(task); receipts.add(id + key); } return task; }};
  const settings = {imap: null, smtp: {}, from: 'support-bot@example.test', replyTo: 'support-bot@example.test', allowedSenders: [], folder: 'INBOX'};
  return {redis, store, settings, tasks};
}
const actor = {type: 'agent', name: 'Support Bot'};
const draft = {to: ['jordan.lee@example.test'], subject: 'Task question', text: 'Which approach?', idempotencyKey: 'question-1'};
const reply = id => ({from: {value: [{address: 'jordan.lee@example.test'}]}, messageId: '<reply-1@example.test>',
  inReplyTo: id, references: [id], subject: 'Re: Task question', text: 'Use the second approach.', headers: new Map()});

test('startup mail probes use isolated connections, authenticate and open inbox read-only without sending or modifying mail state', async t => {
  const {store} = fixture(), settings = mailConfiguration({IMAP_HOST: 'mail.example.test', IMAP_USERNAME: 'agent@example.test', IMAP_PASSWORD: 'hidden', SMTP_HOST: 'mail.example.test', SMTP_USERNAME: 'agent@example.test', SMTP_PASSWORD: 'hidden'});
  const probes = [], transports = [];
  const mailbox = new Mailbox(store, settings, {imapFactory: options => {
    const client = new EventEmitter(); client.close = () => { client.closed = true; };
    client.connect = async () => { assert.equal(options.auth.pass, 'hidden'); };
    client.mailboxOpen = async (folder, options) => { assert.equal(folder, 'INBOX'); assert.equal(options.readOnly, true); };
    probes.push(client); return client;
  }, smtpFactory: options => {
    const transport = {verify: async () => { assert.equal(options.auth.pass, 'hidden'); transport.verified = true; },
      close: () => { transport.closed = true; }, sendMail: () => { assert.fail('Startup check sent email'); }};
    transports.push(transport); return transport;
  }});
  t.after(() => mailbox.close());
  const resident = {}; mailbox.client = resident;
  const before = {...mailbox.state};
  const results = await Promise.all([mailbox.probeInbound(), mailbox.probeOutbound()]);
  assert.ok(results.every(result => result.status === 'available' && result.checkedAt));
  assert.equal(probes[0].closed, true); assert.equal(mailbox.client, resident);
  assert.equal(transports[1].verified, true); assert.equal(transports[1].closed, true);
  assert.equal(transports[0].verified, undefined); assert.equal(transports[0].closed, undefined);
  assert.deepEqual(mailbox.state, before);
  assert.equal(JSON.stringify(results).includes('hidden'), false);
  mailbox.client = null;
});

test('mail startup auth failures are redacted and do not invoke process repair handling', async t => {
  const {store} = fixture(), settings = mailConfiguration({IMAP_HOST: 'mail.example.test', IMAP_USERNAME: 'agent@example.test', IMAP_PASSWORD: 'hidden', SMTP_HOST: 'mail.example.test', SMTP_USERNAME: 'agent@example.test', SMTP_PASSWORD: 'hidden'});
  const mailbox = new Mailbox(store, settings, {imapFactory: () => {
    const client = new EventEmitter(); client.close = () => {};
    client.connect = async () => { throw Object.assign(new Error('password hidden rejected'), {authenticationFailed: true}); }; return client;
  }, smtpFactory: () => ({verify: async () => { throw Object.assign(new Error('AUTH hidden'), {code: 'EAUTH'}); }, close: () => {}}),
  onException: () => assert.fail('Invalid startup credentials created a repair task')});
  t.after(() => mailbox.close());
  const results = await Promise.all([mailbox.probeInbound(), mailbox.probeOutbound()]);
  assert.ok(results.every(result => result.status === 'unavailable' && result.detail === 'Authentication rejected'));
  assert.equal(JSON.stringify(results).includes('hidden'), false);
});

test('Secret environment supports protocol credentials and explicit TLS without endpoint guessing', () => {
  const settings = mailConfiguration({IMAP_HOST: 'imap.example.test', IMAP_PORT: '993', IMAP_USERNAME: 'support-bot', IMAP_PASSWORD: 'hidden',
    SMTP_HOST: 'smtp.example.test', SMTP_PORT: '587', SMTP_USERNAME: 'support-bot', SMTP_PASSWORD: 'hidden', SMTP_FROM: 'support-bot@example.test'});
  assert.equal(settings.imap.secure, true); assert.equal(settings.imap.auth.user, 'support-bot');
  assert.equal(settings.smtp.secure, false); assert.equal(settings.smtp.requireTLS, true);
  assert.equal(mailConfiguration({}).imap, null); assert.equal(mailConfiguration({}).smtp, null);
  const identity = mailConfiguration({IMAP_HOST: 'mail.example.test', SMTP_HOST: 'mail.example.test',
    AGENT_USERNAME: 'alexriver', AGENT_EMAIL: 'alex.river@example.test', IMAP_PASSWORD: 'inbound-secret', SMTP_PASSWORD: 'outbound-secret'});
  assert.equal(identity.imap.auth.user, 'alexriver'); assert.equal(identity.smtp.auth.user, 'alexriver');
  assert.equal(identity.imap.auth.pass, 'inbound-secret'); assert.equal(identity.smtp.auth.pass, 'outbound-secret');
  assert.equal(identity.from, 'alex.river@example.test');
  assert.equal(mailConfiguration({SMTP_FROM: 'other@example.test'}).from, 'other@example.test');
  assert.equal(mailConfiguration({}).saveSent, false);
  assert.equal(mailConfiguration({SMTP_SAVE_SENT: 'true', IMAP_SENT_MAILBOX: 'Sent Items'}).saveSent, true);
  assert.equal(mailConfiguration({SMTP_SAVE_SENT: 'false'}).saveSent, false);
});

test('IMAP and SMTP hosts independently enable their connectors without sharing credentials', async t => {
  for (const protocol of ['IMAP', 'SMTP']) {
    let inbound = 0, outbound = 0;
    const settings = mailConfiguration({[protocol + '_HOST']: 'mail.example.test',
      [protocol + '_PASSWORD']: 'secret', [protocol + '_USERNAME']: 'protocol-user'});
    const gateway = new Mailbox(fixture().store, settings, {
      imapFactory: () => {
        inbound++;
        const client = new EventEmitter(); client.connect = async () => {}; client.mailboxOpen = async () => {};
        client.close = () => {}; return client;
      },
      smtpFactory: () => { outbound++; return {verify: async () => {}, close() {}}; }, log: () => {},
    });
    t.after(() => gateway.close());
    const [incoming, outgoing] = await Promise.all([gateway.probeInbound(), gateway.probeOutbound()]);
    assert.equal(incoming.status, protocol === 'IMAP' ? 'available' : 'disabled');
    assert.equal(outgoing.status, protocol === 'SMTP' ? 'available' : 'disabled');
    assert.equal(inbound, protocol === 'IMAP' ? 1 : 0);
    assert.equal(outbound, protocol === 'SMTP' ? 2 : 0);
  }
  const settings = mailConfiguration({IMAP_HOST: 'imap.example.test', SMTP_HOST: 'smtp.example.test', IMAP_PASSWORD: 'inbound-only'});
  assert.equal(settings.smtp.auth.pass, undefined);
  assert.equal(settings.from, 'agent.smith@system.gov');
  const gateway = new Mailbox(fixture().store, settings, {log: () => {}});
  t.after(() => gateway.close());
  assert.equal(gateway.context().smtp.state, 'incomplete_configuration');
});

test('outbound email binds replies to the original task across restart without GitHub comment side effects', async t => {
  const f = fixture(), sent = [], task = newTask('ns', {type: 'user'}, 'Issue objective', 'webhook',
    [{source: 'github', kind: 'issue', repository: 'org/source', id: '1'}]);
  f.tasks.set(task.id, task); const pending = task.pending.length;
  const gateway = new Mailbox(f.store, f.settings, {transport: {sendMail: async value => { sent.push(value); return {rejected: []}; }}, log: () => {}});
  t.after(() => gateway.close());
  const queued = await gateway.queue(task, actor, draft);
  assert.equal((await gateway.queue(task, actor, draft)).messageId, queued.messageId);
  assert.equal(task.events.filter(e => e.kind === 'email').length, 1);
  gateway.lastLeader = Date.now(); await gateway.flush(); await gateway.flush();
  assert.equal(sent.length, 1); assert.equal(sent[0].messageId, queued.messageId);
  assert.equal((await gateway.taskStatus(task.id)).emails[0].state, 'sent');
  const communication = {primary: {channel: 'github_issue', url: 'https://github.com/org/source/issues/1'}, secondary: ['email', 'chat']};
  assert.deepEqual((await gateway.taskStatus(task.id)).communication, communication);
  assert.equal(gateway.checks['outbound-mail'].status, 'available');
  assert.equal(gateway.checks['outbound-mail'].source, 'operation');
  const restarted = new Mailbox(f.store, f.settings, {transport: {}, log: () => {}}); t.after(() => restarted.close());
  await restarted.receive(reply(queued.messageId), 'validity:7'); await restarted.receive(reply(queued.messageId), 'validity:7');
  assert.equal(task.pending.length, pending + 1);
  const prompt = task.events.at(-1); assert.equal(prompt.actor.type, 'controller');
  const source = task.events.find(e => e.id === prompt.causedBy[0]);
  assert.equal(source.actor.provider, 'email'); assert.equal(source.actor.subject, 'jordan.lee@example.test');
  const other = newTask('ns', {}, 'Other task'); f.tasks.set(other.id, other);
  const messages = await restarted.taskMessages(task.id);
  assert.deepEqual(messages.communication, communication);
  assert.equal(messages.owner, 'codemowers/agent-controller'); assert.equal(messages.scope, 'current_task_threads');
  assert.equal(messages.messages.length, 2); assert.equal(messages.messages[1].direction, 'inbound');
  assert.equal(messages.messages[1].from, 'jordan.lee@example.test');
  assert.match(messages.messages[1].content, /Use the second approach/);
  assert.deepEqual((await restarted.taskMessages(other.id)).messages, []);
  assert.equal(workerIdleTimeoutSeconds(task), 3600);
  assert.equal(sent.length, 1); assert.equal(task.references[0].kind, 'issue');
  const next = await restarted.queue(task, actor, {...draft, idempotencyKey: 'question-2', replyToMessageId: '<reply-1@example.test>'});
  const item = JSON.parse(await f.redis.get(restarted.prefix + 'out:' + next.id));
  assert.equal(item.inReplyTo, '<reply-1@example.test>');
  await restarted.removeTask(task.id); assert.equal(await f.redis.get(restarted.bindingKey(queued.messageId)), null);
  assert.deepEqual((await restarted.taskStatus(task.id)).emails, []);
});

test('unmatched, automated, non-participant and ambiguous replies cannot wake tasks; paused tasks stay paused', async t => {
  const f = fixture(), first = newTask('ns', {}, 'First'), second = newTask('ns', {}, 'Second');
  f.tasks.set(first.id, first); f.tasks.set(second.id, second);
  const gateway = new Mailbox(f.store, f.settings, {transport: {}, log: () => {}}); t.after(() => gateway.close());
  const a = await gateway.queue(first, actor, draft), b = await gateway.queue(second, actor, draft);
  const before = first.pending.length;
  await gateway.receive(reply('<unknown@example.test>'), '1');
  await gateway.receive({...reply(a.messageId), from: {value: [{address: 'stranger@example.test'}]}}, '2');
  await gateway.receive({...reply(a.messageId), headers: new Map([['auto-submitted', 'auto-replied']])}, '3');
  await gateway.receive({...reply(a.messageId), references: [a.messageId, b.messageId]}, '4');
  assert.equal(first.pending.length, before);
  changeState(first, 'Paused', {type: 'user'}); await gateway.receive(reply(a.messageId), '5');
  assert.equal(first.desiredState, 'Paused'); assert.equal(first.pending.length, before + 1);
  await assert.rejects(gateway.queue(first, actor, {...draft, text: 'Changed content'}), {status: 409});
  assert.deepEqual(replyReferences({inReplyTo: a.messageId, references: [a.messageId]}), [a.messageId]);
});

test('uncertain SMTP delivery is durable and never blindly resent', async t => {
  const f = fixture(), task = newTask('ns', {}, 'Send'); f.tasks.set(task.id, task); let calls = 0;
  const gateway = new Mailbox(f.store, f.settings, {transport: {sendMail: async () => { calls++; throw Object.assign(new Error('hidden'), {code: 'ECONNRESET'}); }}, log: () => {}});
  t.after(() => gateway.close()); await gateway.queue(task, actor, draft); gateway.lastLeader = Date.now();
  await gateway.flush(); await gateway.flush(); assert.equal(calls, 1);
  assert.equal((await gateway.taskStatus(task.id)).emails[0].state, 'uncertain');
  assert.equal(gateway.checks['outbound-mail'].status, 'unavailable');
});

test('mail status reports incomplete Secret configuration without inventing mailbox access', async t => {
  const f = fixture(), settings = mailConfiguration({IMAP_HOST: 'mail.example.test', SMTP_HOST: 'mail.example.test'});
  const gateway = new Mailbox(f.store, settings, {log: () => {}}); t.after(() => gateway.close());
  assert.deepEqual(gateway.context().imap, {configured: false, state: 'incomplete_configuration'});
  assert.equal(gateway.context().smtp.configured, false);
  assert.equal(JSON.stringify(gateway.context()).includes('hidden'), false);
});

test('IMAP cursor survives reconnect and UIDVALIDITY reset without duplicating a reply', async t => {
  const f = fixture(), task = newTask('ns', {}, 'Reply'); f.tasks.set(task.id, task);
  const gateway = new Mailbox(f.store, f.settings, {transport: {}, parse: async () => reply(out.messageId), log: () => {}});
  t.after(() => gateway.close()); const out = await gateway.queue(task, actor, draft), before = task.pending.length;
  class Client extends EventEmitter {
    mailbox = {uidValidity: 1n, uidNext: 10};
    async status() { return {uidValidity: this.mailbox.uidValidity, uidNext: this.mailbox.uidNext}; }
    async getMailboxLock() { return {release() {}}; }
    async *fetch(range) { assert.ok(range.startsWith('10:') || range.startsWith('1:')); yield {uid: this.mailbox.uidValidity === 1n ? 10 : 1, size: 20}; }
    async fetchOne() { return {source: Buffer.from('message')}; }
  }
  const client = new Client(); gateway.client = client; gateway.lastLeader = Date.now(); await gateway.drain(client);
  client.mailbox.uidNext = 11; await gateway.drain(client);
  assert.equal(task.pending.length, before + 1);
  client.mailbox = {uidValidity: 2n, uidNext: 2}; await gateway.drain(client);
  assert.equal(task.pending.length, before + 1);
  assert.equal(JSON.parse(await f.redis.get(gateway.prefix + 'cursor')).uidValidity, '2');
  gateway.client = null;
});

test('IDLE wakeup refreshes UIDNEXT and periodic catch-up reads missed notifications exactly once', async t => {
  const f = fixture(), task = newTask('ns', {}, 'Reply'); f.tasks.set(task.id, task);
  const gateway = new Mailbox(f.store, f.settings, {transport: {}, parse: async () => reply(out.messageId), log: () => {}});
  t.after(() => gateway.close()); const out = await gateway.queue(task, actor, draft), before = task.pending.length;
  let statusCalls = 0, nextUid = 11;
  const client = {mailbox: {uidValidity: 1n, uidNext: 10}, close() {},
    async getMailboxLock() { return {release() {}}; },
    async status() { statusCalls++; return {uidValidity: 1n, uidNext: nextUid}; },
    async *fetch(range) { assert.equal(range, '10:10'); yield {uid: 10, size: 30}; },
    async fetchOne() { return {source: Buffer.from('reply')}; }};
  await f.redis.set(gateway.prefix + 'cursor', JSON.stringify({uidValidity: '1', lastUid: 9}));
  gateway.client = client; gateway.state.imap = 'connected';
  // No EXISTS event: the leader tick must catch up using STATUS, even with
  // a stale selected-mailbox UIDNEXT. Normal IDLE also uses this same drain.
  gateway.tick(); await gateway.draining;
  assert.equal(task.pending.length, before + 1); assert.equal(statusCalls, 1);
  assert.equal(JSON.parse(await f.redis.get(gateway.prefix + 'cursor')).lastUid, 10);
  gateway.tick(); assert.equal(statusCalls, 1);
  gateway.nextPoll = 0; gateway.tick(); await gateway.draining;
  assert.equal(statusCalls, 2); assert.equal(task.pending.length, before + 1);
  gateway.client = null;
});

test('IMAP poll exceptions report once, preserve the cursor and recover without terminating the process', async t => {
  const f = fixture(), task = newTask('ns', {}, 'Reply'); f.tasks.set(task.id, task);
  const reports = [], logs = [];
  const faults = installFaultHandler({target: new EventEmitter(), env: {}, log: () => {},
    report: async value => { reports.push(value); return {id: 'repair-task'}; }});
  t.after(() => faults.dispose());
  const gateway = new Mailbox(f.store, f.settings, {transport: {}, log: e => logs.push(e),
    onException: (error, kind) => faults.report(error, kind), parse: async () => reply(out.messageId)});
  t.after(() => gateway.close()); const out = await gateway.queue(task, actor, draft), before = task.pending.length;
  const cursor = JSON.stringify({uidValidity: '1', lastUid: 9}); await f.redis.set(gateway.prefix + 'cursor', cursor);
  let fail = true, closed = 0;
  const client = {mailbox: {uidValidity: 1n, uidNext: 10}, close() { closed++; },
    async getMailboxLock() { return {release() {}}; },
    async status() { if (fail) throw new TypeError('IMAP poll callback broke'); return {uidValidity: 1n, uidNext: 11}; },
    async *fetch() { yield {uid: 10, size: 20}; }, async fetchOne() { return {source: Buffer.from('reply')}; }};
  gateway.lastLeader = Date.now(); gateway.client = client; gateway.state.imap = 'connected';
  await gateway.scheduleDrain(client); await faults.report(new Error('another error'), 'imap_poll');
  assert.equal(reports.length, 1); assert.equal(reports[0].kind, 'imap_poll');
  assert.equal(await f.redis.get(gateway.prefix + 'cursor'), cursor); assert.equal(task.pending.length, before);
  assert.equal(gateway.client, null); assert.equal(closed, 1); assert.ok(gateway.retryAt > Date.now());
  assert.ok(logs.some(e => e.event === 'imap_scan_failed'));
  assert.equal(gateway.checks['inbound-mail'].status, 'unavailable');
  gateway.client = client; await gateway.scheduleDrain(client); assert.equal(reports.length, 1);
  fail = false; gateway.client = client; gateway.state.imap = 'connected';
  await gateway.scheduleDrain(client);
  assert.equal(task.pending.length, before + 1); assert.equal(JSON.parse(await f.redis.get(gateway.prefix + 'cursor')).lastUid, 10);
  assert.equal(gateway.checks['inbound-mail'].status, 'available');
  gateway.client = null;
});

test('Sent copies are gated, recover uncertain APPEND acknowledgements and never resend SMTP', async t => {
  const f = fixture(), task = newTask('ns', {}, 'Mail'); f.tasks.set(task.id, task);
  let sends = 0, appends = 0, searches = 0, failAppend = true, raw;
  const messages = new Set(), selected = [];
  const client = {close() {}, async list() { return [{path: 'Sent', specialUse: '\\Sent'}]; },
    async getMailboxLock(folder) { selected.push(folder); return {release() {}}; },
    async mailboxOpen(folder) { selected.push(folder); },
    async search(query) { searches++; return messages.has(query.header['message-id']) ? [42] : []; },
    async append(folder, source, flags) {
      assert.equal(folder, 'Sent'); assert.deepEqual(flags, ['\\Seen']); appends++; raw = source;
      const mail = await simpleParser(source); messages.add(mail.messageId);
      if (failAppend) throw Object.assign(new Error('Lost APPEND acknowledgement'), {code: 'ECONNRESET'});
      return {uid: 42};
    }};
  const gateway = new Mailbox(f.store, f.settings, {transport: {sendMail: async () => { sends++; return {rejected: []}; }}, log: () => {}});
  t.after(() => gateway.close()); const queued = await gateway.queue(task, actor, draft);
  gateway.client = client; gateway.state.imap = 'connected'; gateway.lastLeader = Date.now();
  await gateway.flush(); assert.equal(sends, 1); assert.equal(appends, 0); assert.equal(searches, 0);
  f.settings.saveSent = true; await gateway.flush();
  const key = gateway.prefix + 'out:' + queued.id;
  let entry = JSON.parse(await f.redis.get(key));
  assert.equal(entry.state, 'sent'); assert.equal(entry.sentCopyState, 'retry_pending');
  assert.equal(sends, 1); assert.equal(appends, 1); assert.deepEqual(selected, ['Sent', 'INBOX']);
  const parsed = await simpleParser(raw); assert.equal(parsed.messageId, queued.messageId);
  assert.equal(parsed.from.value[0].address, 'support-bot@example.test'); assert.match(parsed.text, /Which approach/);
  entry.sentCopyRetryAt = new Date(0).toISOString(); await f.redis.set(key, JSON.stringify(entry));
  gateway.client = client; gateway.state.imap = 'connected'; failAppend = false;
  await gateway.flush(); await gateway.flush();
  entry = JSON.parse(await f.redis.get(key));
  assert.equal(entry.sentCopyState, 'saved'); assert.equal(sends, 1); assert.equal(appends, 1);
  assert.equal(searches, 2); assert.equal((await gateway.taskStatus(task.id)).emails[0].sentCopyState, 'saved');
  gateway.client = null;
});

test('GitHub senders use notification routing without exposing email body as task instructions', async t => {
  const f = fixture(), seen = [];
  const gateway = new Mailbox(f.store, f.settings, {log: () => {}, onGithubNotification: async (mail, delivery) => {
    seen.push({id: mail.messageId, delivery}); return {outcome: 'routed', taskIds: ['task']}; }});
  t.after(() => gateway.close());
  const messageId = '<example-org/sample-repo/issue/42/issue_event/1234567890@github.com>';
  await gateway.receive({from: {value: [{address: 'notifications@github.com'}]}, messageId, text: 'Untrusted email body'}, 'validity:1');
  await gateway.receive({from: {value: [{address: 'noreply@github.com'}]}, messageId}, 'validity:2');
  await gateway.receive({from: {value: [{address: 'notifications@github.com.evil'}]}, messageId}, 'validity:3');
  assert.deepEqual(seen.map(v => v.delivery), ['validity:1', 'validity:2']);
});

test('routing-enabled startup recovers recent GitHub assignments from headers once', async t => {
  const f = fixture(), seen = [];
  const gateway = new Mailbox(f.store, f.settings, {log: () => {}, onGithubNotification: async (mail, delivery, options) => {
    seen.push({id: mail.messageId, delivery, backfill: options.backfill}); return {outcome: 'routed', taskIds: ['new-task']}; }});
  t.after(() => gateway.close());
  const client = {mailbox: {uidValidity: 1n, uidNext: 3}, close() {},
    async getMailboxLock() { return {release() {}}; }, async status() { return {uidValidity: 1n, uidNext: 3}; },
    async *fetch(range, fields) {
      assert.equal(range, '1:2'); assert.ok(fields.headers); assert.equal(fields.source, undefined);
      yield {uid: 1, headers: Buffer.from('From: notifications@github.com\r\nMessage-ID: <example-org/sample-repo/issue/42/issue_event/1234567890@github.com>\r\n\r\n')};
      yield {uid: 2, headers: Buffer.from('From: jordan.lee@example.test\r\nMessage-ID: <ordinary@example.test>\r\n\r\n')};
    }};
  await f.redis.set(gateway.prefix + 'cursor', JSON.stringify({uidValidity: '1', lastUid: 2}));
  gateway.client = client; gateway.lastLeader = Date.now();
  await gateway.drain(client); await gateway.drain(client);
  assert.equal(seen.length, 1); assert.equal(seen[0].backfill, true); assert.equal(seen[0].delivery, '1:1');
  assert.equal(await f.redis.get(gateway.prefix + 'github-routing-v1'), '1');
  assert.equal(JSON.parse(await f.redis.get(gateway.prefix + 'cursor')).lastUid, 2);
  gateway.client = null;
});
