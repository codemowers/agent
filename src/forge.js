import {createHash, createHmac, timingSafeEqual} from 'node:crypto';

const digest = value => createHash('sha256').update(value).digest('hex');
const invalid = message => Object.assign(new Error(message), {status: 400});

export function forgeConfiguration(env = process.env) {
  const provider = env.AGENT_FORGE_PROVIDER || 'github';
  if (!['github', 'gitea'].includes(provider)) throw invalid('Unsupported Git provider');
  const url = new URL(env.AGENT_FORGE_URL || 'https://github.com');
  if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || url.search || url.hash) {
    throw invalid('Expected a Git provider URL without credentials or query parameters');
  }
  const serverURL = url.href.replace(/\/$/, '');
  const apiURL = env.AGENT_FORGE_API_URL || (provider === 'gitea' ? serverURL + '/api/v1'
    : serverURL === 'https://github.com' ? 'https://api.github.com' : serverURL + '/api/v3');
  const api = new URL(apiURL);
  if (!['http:', 'https:'].includes(api.protocol) || api.username || api.password || api.search || api.hash) {
    throw invalid('Expected a Git API URL without credentials or query parameters');
  }
  return {provider, serverURL, apiURL: api.href.replace(/\/$/, '')};
}

export const isForgeReference = reference => ['github', 'gitea'].includes(reference?.source);
export const referenceServer = reference => reference.serverURL || (reference.url ? new URL(reference.url).origin : 'https://github.com');
export const threadKey = reference => 'forge:' + [reference.source, referenceServer(reference), reference.kind,
  reference.repository.toLowerCase(), String(reference.id)].map(encodeURIComponent).join(':');
export const sameForgeRepository = (a, b) => a.source === b.source && referenceServer(a) === referenceServer(b)
  && a.repository?.toLowerCase() === b.repository?.toLowerCase();

export function forgeWebhook(headers, rawBody, config, secret) {
  const prefix = config.provider === 'gitea' ? 'x-gitea-' : 'x-github-';
  const githubSignature = headers['x-hub-signature-256'];
  const signature = config.provider === 'gitea' ? headers['x-gitea-signature']
    : typeof githubSignature === 'string' && githubSignature.startsWith('sha256=') ? githubSignature.slice(7) : undefined;
  const expected = createHmac('sha256', secret || '').update(rawBody || Buffer.alloc(0)).digest('hex');
  return {authenticated: Boolean(secret && typeof signature === 'string' && /^[a-f0-9]{64}$/.test(signature)
      && timingSafeEqual(Buffer.from(signature), Buffer.from(expected))),
    eventType: headers[prefix + 'event'], deliveryId: headers[prefix + 'delivery']};
}

export function forgeReference(url, config = forgeConfiguration()) {
  try {
    const parsed = new URL(url), server = new URL(config.serverURL);
    if (parsed.origin !== server.origin || parsed.username || parsed.password || parsed.search || parsed.hash) return null;
    const prefix = server.pathname.replace(/\/$/, '');
    if (!parsed.pathname.startsWith(prefix + '/')) return null;
    const match = /^\/([\w.-]+)\/([\w.-]+)\/(issues|pull|pulls)\/([1-9]\d*)$/.exec(parsed.pathname.slice(prefix.length));
    if (!match || match[3] === 'pulls' && config.provider !== 'gitea' || match[3] === 'pull' && config.provider === 'gitea') return null;
    return {source: config.provider, serverURL: config.serverURL, repository: `${match[1]}/${match[2]}`,
      kind: match[3] === 'issues' ? 'issue' : 'pr', id: match[4], url: parsed.href};
  } catch { return null; }
}

export async function findThreadTask(store, reference) {
  const key = threadKey(reference), mapping = store.prefix + 'external:' + digest(key);
  const id = await store.redis.get(mapping);
  if (id) {
    try {
      const task = await store.get(id);
      if (task?.references?.some(value => isForgeReference(value) && threadKey(value) === key)) return task;
    } catch (error) { if (error.status !== 404) throw error; }
  }
  // References are the authority; rebuild missing routing indexes from them.
  const task = (await store.list()).find(task => task.references?.some(value => isForgeReference(value) && threadKey(value) === key));
  if (task) await store.redis.set(mapping, task.id);
  return task;
}
