// Startup diagnostics are snapshots, not live health monitors. Never include
// upstream error text: mail servers and HTTP clients may echo credentials.
function failureDetail(error) {
  const code = error.cause?.code || error.code;
  const status = error.upstreamStatus;
  if ([401, 403].includes(status) || error.authenticationFailed || code === 'EAUTH') return 'Authentication rejected';
  if (error.name === 'TimeoutError' || error.name === 'AbortError' || ['ETIMEDOUT', 'UND_ERR_CONNECT_TIMEOUT'].includes(code)) return 'Connection check timed out';
  if (['ENOTFOUND', 'EAI_AGAIN'].includes(code)) return 'Hostname lookup failed';
  if (typeof code === 'string' && /CERT|TLS|SSL/.test(code)) return 'TLS certificate validation failed';
  if (Number.isInteger(status)) return `Service returned HTTP ${status}`;
  return 'Connection or protocol check failed';
}

export async function checkConnection(name, detail, work, {timeoutMs = 15000, onTimeout = () => {}} = {}) {
  let timer;
  try {
    const deadline = new Promise((_, reject) => {
      timer = setTimeout(() => {
        reject(Object.assign(new Error('Check timed out'), {name: 'TimeoutError'}));
        try { onTimeout(); } catch {}
      }, timeoutMs);
    });
    await Promise.race([Promise.resolve().then(work), deadline]);
    return {name, status: 'available', detail, checkedAt: new Date().toISOString(), source: 'startup'};
  } catch (error) {
    return {name, status: 'unavailable', detail: failureDetail(error), checkedAt: new Date().toISOString(), source: 'startup'};
  } finally { clearTimeout(timer); }
}

export function unconfiguredConnection(name, configured) {
  return {name, status: 'disabled', detail: configured ? 'Credentials or configuration incomplete' : 'Not configured',
    checkedAt: new Date().toISOString()};
}

export class ConnectivityCatalog {
  constructor(setup, mailbox, builds) { this.setup = setup; this.mailbox = mailbox; this.builds = builds; }
  start() {
    if (this.pending) return this.pending;
    this.pending = Promise.all([
      this.setup.mcps().catch(() => ['codemowers', 'driftmower', 'agent'].map(name => ({name,
        status: 'unavailable', detail: 'MCP configuration or initialization check failed', checkedAt: new Date().toISOString()}))),
      process.env.GH_TOKEN ? checkConnection('github', 'GitHub API token accepted', () => this.setup.github('/user'))
        : unconfiguredConnection('github', false),
      this.mailbox.probeInbound(), this.mailbox.probeOutbound(),
      this.builds ? this.builds.probe() : unconfiguredConnection('registry', false),
    ]).then(([mcps, github, inbound, outbound, registry]) => ({connections: [...mcps, github, inbound, outbound, registry]}));
    return this.pending;
  }
  async snapshot() {
    const initial = await this.pending;
    return {connections: initial.connections.map(check => this.mailbox.checks?.[check.name] || check)};
  }
}
