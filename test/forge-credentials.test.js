import {test} from 'node:test';
import assert from 'node:assert/strict';
import {execFile} from 'node:child_process';
import {mkdir, mkdtemp, readFile, readlink, rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import http from 'node:http';
import {promisify} from 'node:util';
import {ambiguousEnv, gitProviders, prepareForgeCredentials} from '../src/forge-credentials.js';
import {workerMcpConfig} from '../src/mcp-auth.js';

const exec = promisify(execFile);

test('forge providers outside the operator contract are ignored', () => {
  const env = {AGENT_GIT_PROVIDERS: JSON.stringify([
    {type: 'github', serverURL: 'https://github.com', tokenEnv: 'GH_TOKEN'},
    {type: 'github', serverURL: 'http://plain.example.test', tokenEnv: 'GH_TOKEN'},
    {type: 'svn', serverURL: 'https://svn.example.test', tokenEnv: 'SVN'},
    {type: 'gitea', serverURL: 'https://git.example.test', tokenEnv: 'BAD-NAME'},
  ])};
  assert.deepEqual(gitProviders(env).map(provider => provider.serverURL), ['https://github.com']);
  assert.deepEqual(gitProviders({AGENT_GIT_PROVIDERS: '{'}), []);
});

test('token variables gh and tea apply to every host of a kind are dropped only when ambiguous', () => {
  const one = [{type: 'github', serverURL: 'https://github.com'}, {type: 'github', serverURL: 'https://ghe.example.test'},
    {type: 'gitea', serverURL: 'https://git.example.test'}];
  assert.deepEqual(ambiguousEnv(one), []);
  assert.deepEqual(ambiguousEnv([...one, {type: 'github', serverURL: 'https://ghe.other.test'}]),
    ['GH_ENTERPRISE_TOKEN', 'GITHUB_ENTERPRISE_TOKEN']);
  assert.deepEqual(ambiguousEnv([...one, {type: 'github', serverURL: 'https://team.ghe.com'}]), ['GH_TOKEN', 'GITHUB_TOKEN']);
  assert.deepEqual(ambiguousEnv([...one, {type: 'gitea', serverURL: 'https://git.example.test/two'}]), ['GITEA_INSTANCE_URL']);
});

test('per-host gh, tea and Git credentials stay in the memory volume', async t => {
  const root = await mkdtemp(tmpdir() + '/agent-forge-'); t.after(() => rm(root, {recursive: true, force: true}));
  const dir = root + '/credentials', home = root + '/home';
  await mkdir(home);
  const env = {
    HOME: home, GH_TOKEN: 'dotcom-token', GH_ENTERPRISE_TOKEN: 'one-token', GHE_TWO: 'two-token',
    GITEA_TOKEN: 'gitea-one', GITEA_TWO: 'gitea-two',
    AGENT_GIT_PROVIDERS: JSON.stringify([
      {type: 'github', serverURL: 'https://github.com', tokenEnv: 'GH_TOKEN'},
      {type: 'github', serverURL: 'https://ghe.one.test', tokenEnv: 'GH_ENTERPRISE_TOKEN'},
      {type: 'github', serverURL: 'https://ghe.two.test', tokenEnv: 'GHE_TWO'},
      {type: 'gitea', serverURL: 'https://git.example.test/one', tokenEnv: 'GITEA_TOKEN'},
      {type: 'gitea', serverURL: 'https://git.example.test/two', tokenEnv: 'GITEA_TWO'},
    ]),
  };
  const gitEnv = {...process.env, HOME: home, GIT_CONFIG_NOSYSTEM: '1'};
  const run = (command, args) => exec(command, args, {env: gitEnv});
  const dropped = await prepareForgeCredentials({env, dir, home, giteaUser: 'agent', run});
  assert.deepEqual(dropped, ['GH_ENTERPRISE_TOKEN', 'GITHUB_ENTERPRISE_TOKEN', 'GITEA_INSTANCE_URL']);

  const hosts = await readFile(dir + '/gh/hosts.yml', 'utf8');
  assert.match(hosts, /"ghe\.one\.test":\n {2}user: x-access-token\n {2}oauth_token: "one-token"/);
  assert.match(hosts, /"ghe\.two\.test":\n[^]*"two-token"/);
  assert.equal(env.GH_CONFIG_DIR, dir + '/gh');
  const tea = await readFile(dir + '/tea/config.yml', 'utf8');
  assert.match(tea, /url: "https:\/\/git\.example\.test\/two"\n {4}token: "gitea-two"\n {4}default: false/);
  assert.equal(await readlink(home + '/.config/tea'), dir + '/tea');

  const gitconfig = await readFile(home + '/.gitconfig', 'utf8');
  for (const token of ['dotcom-token', 'one-token', 'two-token', 'gitea-one', 'gitea-two']) {
    assert.ok(!gitconfig.includes(token), `${token} written to the workspace Git config`);
  }
  const fill = async url => (await exec('sh', ['-c', `printf 'url=${url}\\n\\n' | git credential fill`],
    {env: {...gitEnv, GIT_TERMINAL_PROMPT: '0'}})).stdout;
  assert.match(await fill('https://ghe.two.test/team/repo.git'), /username=x-access-token\npassword=two-token\n/);
  assert.match(await fill('https://git.example.test/two/team/repo.git'), /username=agent\npassword=gitea-two\n/);
});

test('credentialed MCP servers read the worker token from its environment', async t => {
  const seen = [];
  const upstream = http.createServer((req, res) => { seen.push(req.headers.authorization); res.writeHead(200); res.end('{}'); });
  await new Promise(resolve => upstream.listen(0, '127.0.0.1', resolve)); t.after(() => upstream.close());
  process.env.MCP_LINEAR_TOKEN = 'linear-token'; t.after(() => { delete process.env.MCP_LINEAR_TOKEN; });
  const config = await workerMcpConfig({mcpServers: {linear: {url: `http://127.0.0.1:${upstream.address().port}/mcp`,
    tokenEnv: 'MCP_LINEAR_TOKEN'}}});
  t.after(() => config.close());
  assert.ok(!JSON.stringify(config.mcpServers).includes('linear-token'));
  await (await fetch(config.mcpServers.linear.url, {method: 'POST', headers: {Authorization: 'Bearer injected'}, body: '{}'})).text();
  assert.deepEqual(seen, ['Bearer linear-token']);
});
