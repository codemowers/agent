import test from 'node:test';
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {generateKeyPair, exportJWK, SignJWT} from 'jose';
import {authentication, authorized} from '../src/auth.js';
import {digest} from '../src/domain.js';

test('API validates access-token signature, type, issuer, audience, expiry and sandbox permission', async t => {
  const {privateKey, publicKey} = await generateKeyPair('RS256');
  const jwk = {...await exportJWK(publicKey), kid: 'test-key', use: 'sig', alg: 'RS256'};
  let issuer;
  const server = createServer((req, res) => {
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify(req.url === '/jwks' ? {keys: [jwk]} : {issuer, jwks_uri: issuer + 'jwks'}));
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(() => server.close());
  issuer = `http://127.0.0.1:${server.address().port}/`;
  const config = {issuer, audience: 'https://agent.example/api', namespace: 'tenant-a',
    allowedGroups: [], allowedSubjects: []};
  const authenticate = await authentication(config);
  const token = (options = {}) => new SignJWT({scope: 'agent:read agent:write',
    'codemowers.io/namespaces': ['tenant-a'], ...options.claims})
    .setProtectedHeader({alg: 'RS256', typ: options.type || 'at+jwt', kid: 'test-key'})
    .setIssuer(options.issuer || issuer).setAudience(options.audience || config.audience)
    .setSubject('user-123').setIssuedAt().setExpirationTime(options.expiry || '1m').sign(options.key || privateKey);
  const request = value => ({headers: {authorization: 'Bearer ' + value}});
  const actor = await authenticate(request(await token()), 'agent:write');
  assert.equal(actor.issuer, issuer); assert.equal(actor.subject, 'user-123');
  for (const options of [{type: 'JWT'}, {issuer: 'https://other.example/'},
    {audience: 'https://other.example/api'}, {expiry: Math.floor(Date.now() / 1000) - 60},
    {key: (await generateKeyPair('RS256')).privateKey}]) {
    await assert.rejects(authenticate(request(await token(options)), 'agent:read'), {status: 401});
  }
  await assert.rejects(authenticate({headers: {}}, 'agent:read'), {status: 401});
  await assert.rejects(authenticate(request(await token({claims: {'codemowers.io/namespaces': ['tenant-b']}})),
    'agent:read'), {status: 403});
  await assert.rejects(authenticate(request(await token({claims: {scope: 'agent:read'}})), 'agent:write'), {status: 403});
});
test('explicit admins allowlist cannot be bypassed with sandbox namespace membership', () => {
  const config={namespace:'llm',allowedGroups:['github.com:codemowers:admins'],allowedSubjects:[]};
  const claims={sub:'user',iss:'https://issuer/',scope:'agent:read',
    'codemowers.io/namespaces':['llm'],groups:['github.com:codemowers:collaborators']};
  assert.throws(()=>authorized(claims,config,'agent:read'),{status:403});
  assert.equal(authorized({...claims,groups:['github.com:codemowers:admins']},config,'agent:read').subject,'user');
  const actor=authorized({...claims,groups:['github.com:codemowers:admins'],email:' Lauri@Example.org ',email_verified:true},config,'agent:read');
  assert.equal(actor.avatarHash,digest('lauri@example.org'));
  assert.equal(actor.email,undefined);
  assert.equal(authorized({...claims,groups:['github.com:codemowers:admins'],email:'lauri@example.org'},config,'agent:read').avatarHash,undefined);
});
