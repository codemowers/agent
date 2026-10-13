import {Problem} from './domain.js';
import {mcpRequest, readMcpConfig} from './mcp-auth.js';
import {checkConnection} from './connectivity.js';
import {agentMcpInitialize} from './agent-mcp.js';

function probeFailure(error) {
  const code = error.cause?.code || error.code;
  if (['TimeoutError', 'AbortError'].includes(error.name) || ['ETIMEDOUT', 'UND_ERR_CONNECT_TIMEOUT'].includes(code)) return 'MCP handshake timed out after 15 seconds';
  if (['ENOENT', 'EACCES'].includes(code)) return 'Projected MCP token file is missing or unreadable';
  if (['ENOTFOUND', 'EAI_AGAIN'].includes(code)) return 'MCP hostname lookup failed';
  if (['ECONNREFUSED', 'ECONNRESET', 'EHOSTUNREACH', 'ENETUNREACH'].includes(code)) return 'MCP network connection failed (' + code + ')';
  if (typeof code === 'string' && /CERT|TLS|SSL/.test(code)) return 'MCP TLS certificate validation failed';
  return 'MCP connection or protocol check failed';
}

export class SetupCatalog {
  constructor(config, request = fetch, mcpConfig = async () => JSON.stringify(await readMcpConfig())) {
    this.config = config; this.request = request; this.mcpConfig = mcpConfig;
  }
  async github(path) {
    const response = await this.request('https://api.github.com' + path, {
      headers: {Authorization: `Bearer ${process.env.GH_TOKEN}`, Accept: 'application/vnd.github+json'},
      signal: AbortSignal.timeout(10000)});
    if (!response.ok) throw Object.assign(new Problem(502, `GitHub discovery returned HTTP ${response.status}`), {upstreamStatus: response.status});
    return {data: await response.json(), scopes: (response.headers.get('x-oauth-scopes') || '').split(',').map(s => s.trim())};
  }
  async repository(name) {
    if (!/^[\w.-]+\/[\w.-]+$/.test(name || '')) throw new Problem(400, 'Expected owner/repository');
    const allow = (process.env.GIT_ALLOWED_REPOSITORIES || '').split(',').filter(Boolean);
    if (allow.length && !allow.includes(name)) throw new Problem(403, 'Repository not authorized');
    const {data} = await this.github('/repos/' + name);
    return data.full_name;
  }
  async githubOverview() {
    const {data: account, scopes} = await this.github('/user');
    const repositories = []; let clipped = false;
    for (let page = 1; page <= 20; page++) {
      const {data} = await this.github(`/user/repos?per_page=100&page=${page}&sort=pushed&affiliation=owner,collaborator,organization_member`);
      repositories.push(...data);
      if (data.length < 100) break;
      if (page === 20) clipped = true;
    }
    const allow = (process.env.GIT_ALLOWED_REPOSITORIES || '').split(',').filter(Boolean);
    const visible = repositories.filter(r => !allow.length || allow.includes(r.full_name));
    const classicCreation = scopes.includes('repo') || scopes.includes('public_repo');
    const destinations = [{owner: account.login, kind: 'user', policy: 'allowed',
      tokenPermission: classicCreation ? 'confirmed' : 'unknown', visibility: scopes.includes('repo') ? 'public/private' : 'public'}];
    const organizations = new Set(repositories.filter(r => r.owner?.type === 'Organization').map(r => r.owner.login));
    try { const {data} = await this.github('/user/orgs?per_page=100'); for (const org of data) organizations.add(org.login); } catch { /* Organization policy may need additional token permissions. */ }
    await Promise.all([...organizations].slice(0, 20).map(async owner => {
      const [org, membership] = await Promise.allSettled([this.github('/orgs/' + owner), this.github('/user/memberships/orgs/' + owner)]);
      const role = membership.status === 'fulfilled' ? membership.value.data.role : undefined;
      const activeMember = membership.status === 'fulfilled' && membership.value.data.state === 'active';
      const policy = org.status === 'fulfilled' ? org.value.data : {};
      const permitted = role === 'admin' || (activeMember && (policy.members_can_create_repositories === true
        || policy.members_can_create_public_repositories === true || policy.members_can_create_private_repositories === true));
      const restricted = activeMember && role !== 'admin' && policy.members_can_create_repositories === false
        && policy.members_can_create_public_repositories !== true && policy.members_can_create_private_repositories !== true;
      destinations.push({owner, kind: 'organization', policy: permitted ? 'allowed' : restricted ? 'restricted' : 'unknown',
        tokenPermission: classicCreation ? 'confirmed' : 'unknown', role});
    }));
    destinations.sort((a,b) => (a.kind === b.kind ? a.owner.localeCompare(b.owner) : a.kind === 'user' ? -1 : 1));
    return {login: account.login, repositoryCount: visible.length, clipped,
      repositories: visible.slice(0, 10).map(r => ({name: r.full_name, url: `https://github.com/${r.full_name}`,
        writable: Boolean(r.permissions?.push), archived: Boolean(r.archived), private: Boolean(r.private)})), destinations};
  }
  async probeMcps() {
    let mcpServers = {}, configurationFailed = false;
    try { ({mcpServers = {}} = JSON.parse(await this.mcpConfig())); } catch { configurationFailed = true; }
    const servers = await Promise.all(Object.entries(mcpServers).map(async ([name, server]) => {
      const result = await this.probeMcp(name, server);
      return {...result, checkedAt: new Date().toISOString()};
    }));
    for (const name of ['codemowers', 'driftmower']) if (!servers.some(server => server.name === name)) servers.push({name,
      status: configurationFailed ? 'unavailable' : 'disabled',
      detail: configurationFailed ? 'MCP configuration unreadable or invalid' : 'Not configured', checkedAt: new Date().toISOString()});
    const agent = await checkConnection('agent', 'Built-in MCP initialization and storage check passed; worker tokens are validated on connection', async () => {
      const initialized = await (this.config.probeAgent || agentMcpInitialize)();
      if (!initialized?.protocolVersion || !initialized.capabilities?.tools) throw new Error('Invalid built-in MCP initialization');
    });
    return [...servers, agent];
  }
  async probeMcp(name, server) {
    if (!server.url) return {name, status: 'unverified', detail: 'Local transport'};
    const url = new URL(server.siteURL || server.url), publicURL = url.origin + url.pathname;
    const started = performance.now();
    try {
      const response = await mcpRequest(server, {method: 'POST', redirect: 'error',
        signal: AbortSignal.timeout(15000), headers: {'Content-Type': 'application/json', Accept: 'application/json, text/event-stream', ...server.headers},
        body: JSON.stringify({jsonrpc: '2.0', id: 1, method: 'initialize', params: {protocolVersion: '2025-03-26',
          capabilities: {}, clientInfo: {name: 'codemowers-agent-status', version: '0.1.0'}}})}, this.request);
      if ([401,403].includes(response.status)) { await response.body?.cancel(); return {name, url: publicURL, status: 'auth_required', detail: 'MCP authentication rejected'}; }
      if (!response.ok) { await response.body?.cancel(); return {name, url: publicURL, status: 'error', detail: `HTTP ${response.status}`}; }
      const reader = response.body.getReader(); let raw = '', initialized = false;
      try {
        while (raw.length < 65536) {
          const {value, done} = await reader.read();
          if (value) raw += new TextDecoder().decode(value, {stream: true});
          const messages = response.headers.get('content-type')?.includes('text/event-stream')
            ? raw.split(/\r?\n\r?\n/).slice(0, -1).map(block => block.split(/\r?\n/).filter(line => line.startsWith('data:')).map(line => line.slice(5).trim()).join('\n')) : [raw];
          for (const message of messages) { try { const rpc = JSON.parse(message); if (rpc.id === 1 && rpc.result?.protocolVersion) initialized = true; } catch {} }
          if (initialized || done) break;
        }
      } finally { try { await reader.cancel(); } catch { /* Cleanup cannot invalidate a received handshake. */ }
        const session = response.headers.get('mcp-session-id');
        if (session) { try { await mcpRequest(server, {method: 'DELETE', redirect: 'error', signal: AbortSignal.timeout(2000), headers: {'Mcp-Session-Id': session}}, this.request); } catch {} }
      }
      return {name, url: publicURL, status: initialized ? 'available' : 'error', detail: initialized ? 'MCP handshake successful' : 'Invalid MCP handshake'};
    } catch (error) {
      const detail = probeFailure(error);
      console.warn(JSON.stringify({level: 'warn', component: 'mcp_probe', name, url: publicURL,
        detail, elapsedMs: Math.round(performance.now() - started)}));
      return {name, url: publicURL, status: 'unavailable', detail};
    }
  }
  async mcps() {
    if (!this.mcpPending) this.mcpPending = this.probeMcps();
    return this.mcpPending;
  }
  async list(force = false) {
    if (!force && this.cached && Date.now() < this.expires) return this.cached;
    this.expires = Date.now() + 60000;
    this.cached = Promise.allSettled([this.githubOverview(), this.mcps()]).then(([github, mcps]) => ({
      github: github.status === 'fulfilled' ? github.value : null,
      mcps: mcps.status === 'fulfilled' ? mcps.value : [],
      warnings: [...(github.status === 'rejected' ? ['GitHub discovery unavailable.'] : []), ...(mcps.status === 'rejected' ? ['MCP configuration unavailable.'] : [])],
      checkedAt: new Date().toISOString()}));
    return this.cached;
  }
}
