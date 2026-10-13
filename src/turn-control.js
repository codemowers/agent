export async function watchTurnControl({endpoint, token, promptIds, interrupt, request = fetch, intervalMs = 1000}) {
  let closed = false, busy = false, interrupted = false;
  const matches = values => Array.isArray(values) && values.length === promptIds.length && new Set(values).size === promptIds.length
    && values.every(id => promptIds.includes(id));
  async function poll() {
    if (closed || busy || interrupted) return;
    busy = true;
    try {
      const response = await request(endpoint + '/control', {headers: {Authorization: `Bearer ${token}`},
        signal: AbortSignal.timeout(3000), redirect: 'error'});
      if (!response.ok) return;
      const control = await response.json();
      if (!closed && matches(control.interrupt?.promptIds)) { interrupted = true; interrupt(); }
    } catch { /* A temporary controller outage must not abort the prompt. */ }
    finally { busy = false; }
  }
  await poll();
  const timer = setInterval(() => void poll(), intervalMs); timer.unref();
  return {close() { closed = true; clearInterval(timer); }};
}
