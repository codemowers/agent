export const agentUserAgent = 'codemowers-agent (+https://github.com/codemowers/agent)';

export function agentFetch(input, options = {}) {
  const headers = new Headers(input instanceof Request ? input.headers : undefined);
  new Headers(options.headers).forEach((value, name) => headers.set(name, value));
  headers.set('User-Agent', agentUserAgent);
  return fetch(input, {...options, headers});
}
