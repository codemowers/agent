import {agentFetch} from './http.js';

export async function watchTurnControl({endpoint, token, promptIds, interrupt, request = agentFetch, intervalMs = 1000}) {
  let closed = false, busy = false, interrupted = false, unavailable = false;
  const matches = values => Array.isArray(values) && values.length === promptIds.length && new Set(values).size === promptIds.length
    && values.every(id => promptIds.includes(id));
  async function poll() {
    if (closed || busy || interrupted) return;
    busy = true;
    try {
      const response = await request(endpoint + '/control', {headers: {Authorization: `Bearer ${token}`},
        signal: AbortSignal.timeout(3000), redirect: 'error'});
      if (!response.ok) {
        if (!unavailable) console.warn(JSON.stringify({event: 'turn_control_unavailable', status: response.status}));
        unavailable = true; return;
      }
      unavailable = false;
      const control = await response.json();
      if (!closed && matches(control.interrupt?.promptIds)) { interrupted = true; console.log(JSON.stringify({event: 'turn_control_interrupt'})); interrupt(); }
    } catch (error) {
      if (!unavailable) console.warn(JSON.stringify({event: 'turn_control_unavailable', reason: error?.name || 'request_failed'}));
      unavailable = true;
    }
    finally { busy = false; }
  }
  await poll();
  const timer = setInterval(() => void poll(), intervalMs); timer.unref();
  return {close() { closed = true; clearInterval(timer); }};
}
