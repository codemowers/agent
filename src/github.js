import {randomUUID} from 'node:crypto';
import {newTask, prompt, activity, controllerActor, terminal} from './domain.js';

export function githubWakeup(eventType, url) {
  const event = {issues: 'issue update', issue_comment: 'issue comment', email: 'email notification',
    pull_request: 'pull request merge'}[eventType] || 'update';
  return `GitHub ${event}: ${url}. Read the latest updates and decide what to do.`;
}

export function pullRequestReference(url) {
  const match = /^https:\/\/github\.com\/([\w.-]+)\/([\w.-]+)\/pull\/([1-9]\d*)$/.exec(url || '');
  return match ? {source: 'github', repository: `${match[1]}/${match[2]}`, kind: 'pr', id: match[3], url} : null;
}

export function reportedPullRequests(task) {
  const references = new Map((task.references || []).filter(r => r.source === 'github' && r.kind === 'pr')
    .map(r => [r.url, r]));
  for (const event of task.events || []) {
    if (event.actor?.type !== 'agent' || !['progress', 'result'].includes(event.kind)) continue;
    for (const url of (event.content || '').match(/https:\/\/github\.com\/[\w.-]+\/[\w.-]+\/pull\/[1-9]\d*/g) || []) {
      if (!references.has(url)) references.set(url, {...pullRequestReference(url), observedAt: event.createdAt});
    }
  }
  return [...references.values()].slice(-20);
}

export function updatePullRequest(task, reference, pull, actor = controllerActor) {
  if (task.deleteRequested || task.completionRequested || terminal.has(task.phase) || task.desiredState === 'Cancelled') return false;
  const previous = task.references.find(r => r.source === 'github' && r.kind === 'pr' && r.url === reference.url);
  const state = pull.merged ? 'merged' : pull.state;
  if (previous?.state === state && previous.mergedAt === pull.mergedAt) return false;
  // On first discovery, do not replay merges already covered by the last reply.
  const baseline = Math.max(Date.parse(reference.observedAt || task.createdAt),
    Date.parse(task.lastExecution?.result?.completedAt || task.lastExecution?.finishedAt || task.createdAt));
  const merged = pull.merged && !previous?.mergeHandled && (previous?.state === 'open' || Date.parse(pull.mergedAt) > baseline);
  const value = {...reference, state, mergedAt: pull.mergedAt, title: pull.title,
    mergeHandled: Boolean(previous?.mergeHandled || pull.merged), checkedAt: new Date().toISOString()};
  if (previous) Object.assign(previous, value); else task.references.push(value);
  if (!merged) return false;
  const source = activity(task, actor, `GitHub pull request merged: ${reference.url}\n${pull.title}`, 'github',
    {kind: 'event', githubEvent: 'pull_request', pullRequest: reference.url, mergedAt: pull.mergedAt});
  prompt(task, controllerActor, githubWakeup('pull_request', reference.url),
    'controller', {causedBy: [source.id]});
  return true;
}

export async function reconcileGithubPullRequests(store, lookup, login, onlyURL) {
  const cache = new Map(), ids = [];
  for (const task of await store.list()) {
    if (task.deleteRequested || task.completionRequested || terminal.has(task.phase) || task.desiredState === 'Cancelled') continue;
    for (const reference of reportedPullRequests(task)) {
      if (onlyURL && reference.url !== onlyURL || reference.mergeHandled) continue;
      try {
        if (!cache.has(reference.url)) cache.set(reference.url, lookup(reference.url));
        const pull = await cache.get(reference.url);
        const existing = task.references.some(r => r.kind === 'pr' && r.url === reference.url);
        // Automatic transcript discovery is limited to PRs authored by this bot.
        if (!existing && pull.author !== login) continue;
        let woke = false;
        await store.mutate(task.id, randomUUID(), current => { woke = updatePullRequest(current, reference, pull); });
        if (woke) ids.push(task.id);
      } catch (error) {
        if (![400, 403, 404, 502].includes(error.status) && !['TimeoutError', 'AbortError', 'TypeError'].includes(error.name)) throw error;
        console.error(JSON.stringify({event: 'github_pr_reconcile_failed', taskId: task.id, repository: reference.repository, pullRequest: reference.id}));
      }
    }
  }
  return [...new Set(ids)];
}

export function closeGithubTask(task, actor, url) {
  if (task.deleteRequested || task.desiredState === 'Cancelled' || task.phase === 'Completed' || task.completionRequested) return;
  task.githubIssueState = 'closed'; task.completionRequested = true;
  task.desiredState = 'Paused'; task.phase = task.active ? 'Stopping' : 'Completed';
  task.reason = 'GitHubIssueClosed';
  if (task.active) task.active.stopRequested = true;
  activity(task, actor, `GitHub issue closed: ${url}`, 'controller', {kind: 'control'});
}

export async function reconcileGithubIssues(store, lookup) {
  for (const task of await store.list()) {
    if (task.deleteRequested || task.desiredState === 'Cancelled' || task.phase === 'Completed' || task.completionRequested) continue;
    const reference = task.references?.find(r => r.source === 'github' && r.kind === 'issue');
    if (!reference) continue;
    try {
      const issue = await lookup(reference.url);
      if (issue.state !== 'closed') continue;
      await store.mutate(task.id, `github-closed:${reference.repository}:${reference.id}:${issue.closedAt || 'closed'}`,
        current => closeGithubTask(current, controllerActor, reference.url));
      console.log(JSON.stringify({event: 'github_issue_reconciled', taskId: task.id,
        repository: reference.repository, issue: reference.id, state: 'closed'}));
    } catch {
      // One inaccessible repository must not prevent other task/Pod reconciliation.
      console.error(JSON.stringify({event: 'github_issue_reconcile_failed', taskId: task.id,
        repository: reference.repository, issue: reference.id}));
    }
  }
}

export function acceptsGithubEvent(eventType, payload, login) {
  if (!['issues', 'issue_comment'].includes(eventType) || !payload.issue || payload.issue.pull_request) return false;
  // Assigning oneself is an explicit request to start work. Ignore other bot
  // activity so its own comments do not cause recursive wakeups.
  if (payload.sender?.login === login && !(eventType === 'issues' && payload.action === 'assigned')) return false;
  if (!payload.issue.assignees?.some(user => user.login === login)) return false;
  return eventType === 'issues'
    ? ['opened', 'reopened', 'assigned', 'labeled'].includes(payload.action)
    : payload.action === 'created';
}

export function githubTask(namespace, actor, issue, eventType, context, delivery) {
  const task = newTask(namespace, actor, issue.content, 'webhook', [issue.reference]);
  task.title = String(issue.title || issue.content.split('\n')[0]).slice(0, 200);
  task.repository = issue.repository;
  // The initial source event is context, not a second dispatchable prompt.
  const source = task.events[0]; source.kind = 'event'; source.content = context;
  source.deliveryId = delivery;
  task.pending = [];
  prompt(task, controllerActor, githubWakeup(eventType, issue.reference.url), 'controller', {causedBy: [source.id]});
  return task;
}
