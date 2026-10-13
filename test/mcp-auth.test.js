import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp, writeFile, rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import http from 'node:http';
import {workerMcpConfig, readMcpConfig, driftmowerTokenProjection} from '../src/mcp-auth.js';
import {Kube} from '../src/kube.js';
import {newTask, reserve} from '../src/domain.js';

test('optional MCP configuration disables remote servers when absent and rejects malformed input', async t => {
  const root = await mkdtemp(tmpdir() + '/agent-mcp-config-'); t.after(() => rm(root, {recursive: true, force: true}));
  const path = root + '/mcp.json';
  assert.deepEqual(await readMcpConfig(path), {mcpServers: {}});
  await writeFile(path, JSON.stringify({mcpServers: {advisory: {type: 'http', url: 'https://advisory.example.test/mcp'}}}));
  assert.equal((await readMcpConfig(path)).mcpServers.advisory.url, 'https://advisory.example.test/mcp');
  await writeFile(path, '{');
  await assert.rejects(readMcpConfig(path), SyntaxError);
});

test('workers mount optional external MCP configuration and a stable audience-bound token without embedded credentials', () => {
  const task = newTask('sandbox', {}, 'Work'); reserve(task);
  const pod = new Kube({namespace: 'sandbox', release: 'agent', image: 'runner', githubSecret: 'github'}).pod(task);
  assert.deepEqual(pod.spec.volumes.find(volume => volume.name === 'mcp-config').configMap,
    {name: 'agent-mcp', optional: true});
  assert.deepEqual(pod.spec.volumes.find(volume => volume.name === 'mcp-token').projected.sources,
    [{serviceAccountToken: driftmowerTokenProjection}]);
  assert.equal(pod.spec.containers[0].env.find(entry => entry.name === 'AGENT_MCP_CONFIG_FILE').value, '/mcp/mcp.json');
});

test('worker MCP bridge reloads rotated tokens, preserves sessions and never puts credentials in CLI config', async t => {
  const root = await mkdtemp(tmpdir() + '/agent-mcp-'); t.after(() => rm(root, {recursive:true,force:true}));
  const tokenFile = root + '/token'; await writeFile(tokenFile, 'first');
  const seen = [];
  const upstream = http.createServer(async (req, res) => {
    seen.push({authorization:req.headers.authorization,session:req.headers['mcp-session-id']});
    res.writeHead(200, {'content-type':'application/json','mcp-session-id':'session'});res.end('{"ok":true}');
  });
  await new Promise(resolve=>upstream.listen(0,'127.0.0.1',resolve));t.after(()=>upstream.close());
  const config = await workerMcpConfig({mcpServers:{driftmower:{url:`http://127.0.0.1:${upstream.address().port}/mcp`,tokenFile,siteURL:'https://console.example.test/'}}});
  t.after(()=>config.close());
  const url = config.mcpServers.driftmower.url;
  assert.ok(!JSON.stringify(config.mcpServers).includes('first'));
  assert.ok(!JSON.stringify(config.mcpServers).includes(tokenFile));
  assert.ok(!JSON.stringify(config.mcpServers).includes('console.example.test'));
  const first = await fetch(url,{method:'POST',headers:{'content-type':'application/json'},body:'{}'});
  assert.equal(first.headers.get('mcp-session-id'),'session');await first.text();
  await writeFile(tokenFile,'rotated');
  const second = await fetch(url,{method:'POST',headers:{'mcp-session-id':'session',Authorization:'Bearer injected'},body:'{}'});await second.text();
  assert.deepEqual(seen,[{authorization:'Bearer first',session:undefined},{authorization:'Bearer rotated',session:'session'}]);
  assert.equal((await fetch(url.replace('/mcp','/elsewhere'))).status,404);
});
