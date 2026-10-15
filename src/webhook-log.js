import {randomUUID} from 'node:crypto';

// Log only routing identifiers. Never serialize bodies, headers, query strings,
// URLs, alert labels/annotations, issue text, or user-supplied error messages.
function identifier(value, pattern = /^[\w.-]+$/, max = 200) {
  return typeof value === 'string' && value.length <= max && pattern.test(value) ? value : undefined;
}
function metadata(provider, body) {
  if (!body || typeof body !== 'object') return {};
  if (['github', 'gitea'].includes(provider)) return {
    action: identifier(body.action), repository: identifier(body.repository?.full_name, /^[\w.-]+\/[\w.-]+$/),
    isPullRequest: Boolean(body.issue?.pull_request || body.is_pull || body.pull_request),
    issue: Number.isSafeInteger(body.issue?.number) ? body.issue.number : undefined,
    pullRequest: Number.isSafeInteger(body.pull_request?.number) ? body.pull_request.number : undefined,
    merged: typeof body.pull_request?.merged === 'boolean' ? body.pull_request.merged : undefined,
    sender: identifier(body.sender?.login),
    assignees: Array.isArray(body.issue?.assignees)
      ? body.issue.assignees.slice(0, 20).map(a => identifier(a?.login)).filter(Boolean) : undefined,
  };
  if (provider === 'alertmanager') return {
    alertStatus: ['firing', 'resolved'].includes(body.status) ? body.status : undefined,
    alertCount: Array.isArray(body.alerts) ? body.alerts.length : undefined,
  };
  return {}; // Wakeup context/delivery IDs may be arbitrary user text.
}

export function webhookLog(write = entry => console.log(JSON.stringify(entry))) {
  return (req, res, next) => {
    req.webhookLogged = true;
    const provider = {'/hooks/github': req.headers['x-gitea-event'] ? 'gitea' : 'github', '/hooks/wakeup': 'wakeup', '/hooks/alertmanager': 'alertmanager'}[req.baseUrl + req.path] || 'unknown';
    const started = Date.now();
    const common = {component: 'webhook', requestId: randomUUID(), provider, method: req.method,
      deliveryId: ['github', 'gitea'].includes(provider) ? identifier(req.headers['x-gitea-delivery'] || req.headers['x-github-delivery']) : undefined,
      eventType: ['github', 'gitea'].includes(provider) ? identifier(req.headers['x-gitea-event'] || req.headers['x-github-event']) : undefined};
    write({...common, event: 'webhook_received'});
    res.once('finish', () => write({...common, event: 'webhook_completed', status: res.statusCode,
      durationMs: Date.now() - started, authenticated: Boolean(res.locals.webhookAuthenticated),
      outcome: res.locals.webhookOutcome || (res.statusCode >= 500 ? 'failed' : res.statusCode >= 400 ? 'rejected' : 'accepted'),
      reason: res.locals.webhookReason,
      ...(res.locals.webhookAuthenticated ? metadata(provider, req.body) : {}),
      taskId: res.locals.webhookTaskId, taskIds: res.locals.webhookTaskIds}));
    next();
  };
}
