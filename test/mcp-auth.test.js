import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp, writeFile, rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import http from 'node:http';
import {workerMcpConfig} from '../src/mcp-auth.js';

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
