import test from 'node:test';
import assert from 'node:assert/strict';
import express from 'express';
import {once} from 'node:events';
import {webhookLog} from '../src/webhook-log.js';

test('webhooks log hits before JSON parsing and allowlisted routing metadata without credentials or content', async t => {
  const records = [], app = express();
  app.use('/hooks', webhookLog(entry => records.push(entry)));
  app.use(express.json());
  app.post('/hooks/github', (req, res) => {
    if (req.headers.authorization !== 'Bearer secret-token') { res.status(401).end(); return; }
    res.locals.webhookAuthenticated = true;
    res.locals.webhookOutcome = 'ignored'; res.locals.webhookReason = 'not_assigned_to_agent';
    res.status(202).json({ignored: true});
  });
  app.post('/hooks/alertmanager', (req, res) => {
    res.locals.webhookAuthenticated = true; res.status(202).end();
  });
  app.use((error, req, res, next) => res.status(400).end());
  const server = app.listen(0, '127.0.0.1');
  t.after(() => { server.closeAllConnections(); server.close(); });
  await once(server, 'listening');
  const base = `http://127.0.0.1:${server.address().port}`;
  const headers = {'content-type': 'application/json', authorization: 'Bearer secret-token',
    'x-hub-signature-256': 'sha256=secret-signature', 'x-github-delivery': 'delivery-123', 'x-github-event': 'issues'};
  const body = {action: 'assigned', repository: {full_name: 'codemowers/pitchdeck', secret: 'secret-repository'},
    issue: {number: 1, title: 'secret-title', body: 'secret-body', assignees: [{login: 'agent-smith'}]},
    sender: {login: 'alexriver'}, token: 'secret-body-token'};
  assert.equal((await fetch(base + '/hooks/github?token=secret-query', {method: 'POST', headers, body: JSON.stringify(body)})).status, 202);
  assert.equal(records[0].event, 'webhook_received');
  assert.equal(records[0].provider, 'github');
  assert.equal(records[1].requestId, records[0].requestId);
  assert.equal(records[1].repository, 'codemowers/pitchdeck');
  assert.deepEqual(records[1].assignees, ['agent-smith']);
  assert.equal(records[1].outcome, 'ignored');
  assert.equal((await fetch(base + '/hooks/github', {method: 'POST', headers: {'content-type': 'application/json'}, body: JSON.stringify(body)})).status, 401);
  assert.equal(records[3].authenticated, false);
  assert.equal(records[3].repository, undefined);
  assert.equal(records[3].outcome, 'rejected');
  assert.equal((await fetch(base + '/hooks/github', {method: 'POST', headers, body: 'secret-malformed-json'})).status, 400);
  assert.equal(records[5].status, 400);
  await fetch(base + '/hooks/alertmanager', {method: 'POST', headers, body: JSON.stringify({status: 'firing', alerts: [{labels: {token: 'secret-alert'}, annotations: {text: 'secret-annotation'}}]})});
  assert.equal(records[7].alertCount, 1);
  assert.equal(records[7].status, 202);
  assert.equal(JSON.stringify(records).includes('secret-'), false);
});
