import test from 'node:test';
import assert from 'node:assert/strict';
import {SetupCatalog} from '../src/setup.js';

test('setup clips repositories, distinguishes org policy from token grants, and probes configured MCP without exposing secrets', async () => {
  let requests = 0;
  const request = async url => {
    requests++;
    const path = new URL(url).pathname;
    const json = data => new Response(JSON.stringify(data), {headers: {'content-type':'application/json'}});
    if (path === '/user') return json({login:'agent-smith'});
    if (path === '/user/repos') return json(Array.from({length:11},(_,i)=>({full_name:'codemowers/repo-'+i,owner:{login:'codemowers',type:'Organization'},permissions:{push:true}})));
    if (path === '/user/orgs') return json([{login:'codemowers'}]);
    if (path === '/orgs/codemowers') return json({members_can_create_public_repositories:true});
    if (path === '/user/memberships/orgs/codemowers') return json({state:'active',role:'member'});
    if (path === '/repos/codemowers/repo-1') return json({full_name:'codemowers/repo-1'});
    if (path === '/auth-mcp') return new Response('',{status:401});
    if (path === '/available-mcp') return json({jsonrpc:'2.0',id:1,result:{protocolVersion:'2025-03-26'}});
    throw Error('Unexpected path '+path);
  };
  const catalog = new SetupCatalog({},request,async()=>JSON.stringify({mcpServers:{
    protected:{url:'https://mcp.example/auth-mcp?token=secret-query',siteURL:'https://console.example.test/',headers:{Authorization:'Bearer secret-header'}},
    ready:{url:'https://mcp.example/available-mcp'},
  }}));
  const setup = await catalog.list();
  assert.equal(setup.github.repositories.length,10);assert.equal(setup.github.repositoryCount,11);
  const organization=setup.github.destinations.find(d=>d.owner==='codemowers');
  assert.equal(organization.policy,'allowed');assert.equal(organization.tokenPermission,'unknown');
  assert.equal(setup.mcps.find(m=>m.name==='protected').status,'auth_required');
  assert.equal(setup.mcps.find(m=>m.name==='protected').url,'https://console.example.test/');
  assert.equal(setup.mcps.find(m=>m.name==='ready').status,'available');
  assert.equal(JSON.stringify(setup).includes('secret-'),false);
  const before=requests;await catalog.list();assert.equal(requests,before);
  await catalog.list(true);assert.ok(requests>before);
  assert.equal(await catalog.repository('codemowers/repo-1'),'codemowers/repo-1');
  await assert.rejects(catalog.repository('https://github.com/untrusted/repo'),{status:400});
});
