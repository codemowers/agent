import {test} from 'node:test';
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {createMailboxMcpServer, audience} from '../mailbox/mcp.js';

async function listen(server, t) {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(() => new Promise(resolve => server.close(resolve)));
  return `http://127.0.0.1:${server.address().port}`;
}

test('Mailbox MCP authenticates ServiceAccount tokens and checks RBAC verbs per call', async t => {
  const reviews = [];
  const kube = async (path, body) => {
    reviews.push({path, body});
    if (path.endsWith('/tokenreviews')) {
      assert.deepEqual(body.spec.audiences, [audience]);
      const username = {good: 'system:serviceaccount:team:agent-worker-mat', foreign: 'system:serviceaccount:other:agent-worker-mat'}[body.spec.token];
      return {status: username ? {authenticated: true, user: {username, uid: 'u', groups: []}} : {authenticated: false}};
    }
    const {verb, name, resource, namespace} = body.spec.resourceAttributes;
    assert.deepEqual([resource, name, namespace], ['mailboxes', 'shared', 'team']);
    return {status: {allowed: verb === 'send'}};
  };
  const sent = [];
  const smtp = createServer(async (req, res) => {
    const chunks = [];
    for await (const chunk of req) chunks.push(chunk);
    sent.push(JSON.parse(Buffer.concat(chunks)));
    res.writeHead(200, {'content-type': 'application/json'}).end(JSON.stringify({messageId: '<m@test>', accepted: ['to@example.test'], rejected: [], sentCopy: {status: 'disabled'}}));
  });
  const smtpURL = await listen(smtp, t);
  const url = await listen(createMailboxMcpServer({namespace: 'team', mailbox: 'shared', address: 'shared@example.test',
    smtp: true, imap: true, kube, smtpURL, listURL: 'http://127.0.0.1:1'}), t);
  const rpc = (token, method, params) => fetch(`${url}/mcp`, {method: 'POST', headers: {authorization: `Bearer ${token}`},
    body: JSON.stringify({jsonrpc: '2.0', id: 1, method, params})});

  assert.equal((await rpc('bad', 'tools/list')).status, 401);
  assert.equal((await rpc('foreign', 'tools/list')).status, 401);
  const init = await (await rpc('good', 'initialize')).json();
  assert.match(init.result.instructions, /may send mail\./);
  const tools = await (await rpc('good', 'tools/list')).json();
  assert.deepEqual(tools.result.tools.map(tool => tool.name), ['send_email', 'list_mail_folders', 'list_mailbox_messages', 'read_mail_message']);
  const denied = await (await rpc('good', 'tools/call', {name: 'list_mail_folders', arguments: {}})).json();
  assert.equal(denied.result.isError, true);
  const result = await (await rpc('good', 'tools/call', {name: 'send_email',
    arguments: {to: ['to@example.test'], subject: 'Hi', text: 'Hello'}})).json();
  assert.equal(result.result.isError, false);
  assert.equal(JSON.parse(result.result.content[0].text).submitted, true);
  assert.deepEqual(sent, [{to: ['to@example.test'], subject: 'Hi', text: 'Hello'}]);
});

test('Mailbox MCP pinned with agentRef accepts only that Agent\'s worker', async t => {
  const kube = async (path, body) => {
    if (path.endsWith('/tokenreviews')) {
      const username = {mat: 'system:serviceaccount:team:agent-worker-mat', krtek: 'system:serviceaccount:team:agent-worker-krtek'}[body.spec.token];
      return {status: {authenticated: true, user: {username, uid: 'u', groups: []}}};
    }
    return {status: {allowed: true}};
  };
  const url = await listen(createMailboxMcpServer({namespace: 'team', mailbox: 'shared', smtp: true,
    caller: 'system:serviceaccount:team:agent-worker-mat', kube}), t);
  const list = token => fetch(`${url}/mcp`, {method: 'POST', headers: {authorization: `Bearer ${token}`},
    body: JSON.stringify({jsonrpc: '2.0', id: 1, method: 'tools/list'})});
  assert.equal((await list('krtek')).status, 401);
  assert.equal((await list('mat')).status, 200);
});
