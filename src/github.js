import {forgeReference, forgeConfiguration, isForgeReference, threadKey, sameForgeRepository, findThreadTask} from './forge.js';
import {randomUUID} from 'node:crypto';
import {newTask, prompt, activity, controllerActor, terminal, digest} from './domain.js';

export function githubWakeup(eventType, url) {
  const event = {issues: 'issue update', issue_comment: 'issue comment', email: 'email notification',
    pull_request: 'pull request merge', pull_request_assigned: 'pull request assignment',
    pull_request_comment: 'pull request comment', pull_request_update: 'pull request update'}[eventType] || 'update';
  return `Git ${event}: ${url}. Read the latest updates and decide what to do.`;
}

export function restoreGithubIssue(task, issue) {
  for (const reference of task.references || []) if (isForgeReference(reference) && sameForgeRepository(reference, issue.reference)) {
    delete reference.unavailable;
  }
  if (task.githubIssueState === 'unavailable') {
    task.githubIssueState = issue.state;
    if (['GitHubIssueUnavailable', 'GitHubRepositoryUnavailable'].includes(task.reason)) task.reason = null;
  }
}

function unavailableGithubIssue(task, reference, error) {
  if (task.deleteRequested || task.completionRequested || terminal.has(task.phase) || task.desiredState === 'Cancelled') return;
  const refs = (task.references || []).filter(value => isForgeReference(value)
    && (error.repositoryUnavailable ? sameForgeRepository(value, reference) : threadKey(value) === threadKey(reference)));
  if (!refs.length || refs.every(value => value.unavailable)) return;
  for (const value of refs) value.unavailable = {status: error.status,
    scope: error.repositoryUnavailable ? 'repository' : 'issue', checkedAt: new Date().toISOString()};
  task.githubIssueState = 'unavailable';
  task.reason = error.repositoryUnavailable ? 'GitHubRepositoryUnavailable' : 'GitHubIssueUnavailable';
  activity(task, controllerActor, `GitHub ${error.repositoryUnavailable ? 'repository' : 'issue'} unavailable (HTTP ${error.status}): ${error.repositoryUnavailable ? reference.repository : reference.url}. Automatic polling stopped; the task and workspace are retained.`,
    'controller', {kind: 'recovery'});
}

export function githubReference(url, config) { return forgeReference(url, config); }

export function pullRequestReference(url, config) {
  const reference = githubReference(url, config);
  return reference?.kind === 'pr' ? reference : null;
}

export function githubEventURL(reference, candidate) {
  if (!candidate) return reference.url;
  try {
    const target = new URL(candidate), base = new URL(reference.url);
    const path = base.pathname.replace(/\/(?:issues|pull|pulls)\/\d+$/, '');
    if (target.origin === base.origin && !target.username && !target.password && !target.search
      && target.hash && ['issues', 'pull', 'pulls'].some(kind => target.pathname === `${path}/${kind}/${reference.id}`))
      return target.href;
  } catch { /* Fall back to the verified thread URL. */ }
  return reference.url;
}

export async function routeGithubIssueEvent(store, namespace, issue, login, options) {
  const existing = await findThreadTask(store, issue.reference);
  const {actor, eventType, action, deliveryId} = options;
  if (eventType === 'issue_comment' && actor.name?.toLowerCase() === login.toLowerCase()) {
    return {outcome: 'ignored', reason: 'agent_authored_event'};
  }
  if (!existing && (issue.state !== 'open' || !issue.assignees?.some(value => value.toLowerCase() === login.toLowerCase()))) {
    return {outcome: 'ignored', reason: issue.state !== 'open' ? 'closed_without_task' : 'not_assigned_to_agent'};
  }
  const key = threadKey(issue.reference), context = options.comment || issue.content;
  const proposed = githubTask(namespace, actor, {...issue, commentURL: options.commentURL}, eventType, context, deliveryId);
  const task = existing || await store.create(proposed, 'external:' + key);
  await store.redis.set(store.prefix + 'external:' + digest(key), task.id);
  if (task.id === proposed.id) return {outcome: 'created', taskId: task.id};
  let outcome = 'duplicate';
  await store.mutate(task.id, 'git-hook:' + deliveryId, current => {
    if (current.deleteRequested || current.desiredState === 'Cancelled') return;
    if (issue.state === 'closed') { closeGithubTask(current, actor, issue.reference.url); outcome = 'completed'; return; }
    if (eventType === 'issues' && action === 'reopened') reopenGithubTask(current, actor, issue.reference.url);
    if (current.completionRequested || terminal.has(current.phase)) return;
    restoreGithubIssue(current, issue);
    if (current.events.some(event => event.deliveryId === deliveryId)) return;
    const update = options.comment || `Issue ${action || 'updated'}: ${issue.reference.url}`;
    const source = activity(current, actor, update, issue.reference.source, {kind: 'event', deliveryId,
      externalURL: githubEventURL(issue.reference, options.commentURL)});
    prompt(current, controllerActor, githubWakeup(eventType, issue.reference.url), 'controller',
      {notificationKey: key, causedBy: [source.id]});
    outcome = current.desiredState === 'Paused' ? 'queued' : 'resumed';
  });
  return {outcome, taskId: task.id};
}

export async function routeGithubPullRequest(store, namespace, pull, login, options = {}) {
  const {reference} = pull;
  const actor = options.actor || controllerActor;
  if (options.commentAction && actor.name?.toLowerCase() === login.toLowerCase() && !options.assignment) return {outcome: 'ignored', reason: 'agent_authored_event', taskIds: []};
  const tasks = (await store.list()).filter(task => reportedPullRequests(task).some(value => threadKey(value) === threadKey(reference))
    && (task.references.some(value => value.kind === 'pr' && threadKey(value) === threadKey(reference))
      || pull.author?.toLowerCase() === login.toLowerCase()));
  if (!tasks.length) {
    if (pull.state !== 'open') return {outcome: 'ignored', reason: 'pull_request_closed', taskIds: []};
    if (!pull.assignees?.some(value => value.toLowerCase() === login.toLowerCase())) {
      return {outcome: 'ignored', reason: 'not_assigned_to_agent', taskIds: []};
    }
    const key = threadKey(reference);
    const delivery = options.deliveryId || key;
    const comment = options.comment;
    const proposed = githubTask(namespace, actor, {...pull, commentURL: options.commentURL}, comment ? 'pull_request_comment' : 'pull_request_assigned',
      comment || pull.content, delivery);
    Object.assign(proposed.references[0], {state: pull.state, observedAt: proposed.createdAt});
    const task = await store.create(proposed, 'external:' + key);
    await store.redis.set(store.prefix + 'external:' + digest(key), task.id);
    if (task.id === proposed.id) return {outcome: 'created', taskIds: [task.id]};
    tasks.push(task);
  }
  if (options.discovery) return {outcome: 'duplicate', taskIds: tasks.map(task => task.id)};
  const ids = [], duplicates = [];
  for (const task of tasks) {
    if (task.deleteRequested || task.completionRequested || terminal.has(task.phase) || task.desiredState === 'Cancelled') continue;
    const delivery = options.deliveryId || 'github-pr:' + randomUUID();
    if (task.events.some(event => event.deliveryId === delivery)) { duplicates.push(task.id); continue; }
    await store.mutate(task.id, delivery, current => {
      if (current.deleteRequested || current.completionRequested || terminal.has(current.phase) || current.desiredState === 'Cancelled') return;
      if (current.events.some(event => event.deliveryId === delivery)) return;
      if (!current.references.some(value => value.url === reference.url)) current.references.push({...reference, state: pull.state});
      restoreGithubIssue(current, pull);
      const source = activity(current, actor, options.comment || `Pull request ${options.commentAction || 'updated'}: ${reference.url}`,
        reference.source, {kind: 'event', deliveryId: delivery, externalURL: githubEventURL(reference, options.commentURL)});
      prompt(current, controllerActor, githubWakeup(options.commentAction ? 'pull_request_comment' : 'pull_request_update', reference.url),
        'controller', {notificationKey: threadKey(reference), causedBy: [source.id]});
    });
    ids.push(task.id);
  }
  const active = tasks.some(task => ids.includes(task.id) && task.desiredState !== 'Paused');
  return {outcome: ids.length ? active ? 'resumed' : 'queued' : duplicates.length ? 'duplicate' : 'ignored',
    reason: ids.length || duplicates.length ? undefined : 'task_closed', taskIds: [...ids, ...duplicates]};
}

export async function reconcileGithubAssignments(store, namespace, references, lookup, login) {
  const known = new Set((await store.list()).flatMap(task => (task.references || []).filter(value => isForgeReference(value)).map(threadKey)));
  const missing = references.filter(reference => !known.has(threadKey(reference))).slice(0, 20);
  let created = 0;
  for (const reference of missing) {
    try {
      const issue = await lookup(reference);
      if (issue.state !== 'open' || !issue.assignees?.some(value => value.toLowerCase() === login.toLowerCase())) continue;
      if (reference.kind === 'pr') {
        const result = await routeGithubPullRequest(store, namespace, issue, login, {discovery: true});
        if (result.outcome === 'created') created++;
      } else {
        const key = threadKey(reference);
        const proposed = githubTask(namespace, controllerActor, issue, 'issues', issue.content, 'assignment-discovery:' + key);
        const task = await store.create(proposed, 'external:' + key);
        await store.redis.set(store.prefix + 'external:' + digest(key), task.id);
        if (task.id === proposed.id) created++;
      }
    } catch (error) {
      console.error(JSON.stringify({event: 'github_assignment_lookup_failed', repository: reference.repository, kind: reference.kind, number: reference.id,
        ...(Number.isInteger(error.status) ? {status: error.status} : {})}));
    }
  }
  return {created, remaining: Math.max(0, references.filter(reference => !known.has(threadKey(reference))).length - missing.length)};
}

export function reportedPullRequests(task, config = forgeConfiguration()) {
  const references = new Map((task.references || []).filter(r => isForgeReference(r) && r.kind === 'pr')
    .map(r => [r.url, r]));
  for (const event of task.events || []) {
    if (event.actor?.type !== 'agent' || !['progress', 'result'].includes(event.kind)) continue;
    for (const url of (event.content || '').match(/https?:\/\/[^\s<>"`)]+/g) || []) {
      const reference = pullRequestReference(url.replace(/[.,;]+$/, ''), config);
      if (reference && !references.has(reference.url)) references.set(reference.url, {...reference, observedAt: event.createdAt});
    }
  }
  return [...references.values()].slice(-20);
}

export function updatePullRequest(task, reference, pull, actor = controllerActor) {
  if (task.deleteRequested || task.completionRequested || terminal.has(task.phase) || task.desiredState === 'Cancelled') return false;
  const previous = task.references.find(r => isForgeReference(r) && r.kind === 'pr' && r.url === reference.url);
  const state = pull.merged ? 'merged' : pull.state;
  if (previous?.state === state && previous.mergedAt === pull.mergedAt) return false;
  // An old link is context, not a new merge event. This is reference lifecycle
  // state, independent of what the model has read or when it last replied.
  const baseline = Date.parse(reference.observedAt || task.createdAt);
  const merged = pull.merged && !previous?.mergeHandled && (previous?.state === 'open' || Date.parse(pull.mergedAt) > baseline);
  const value = {...reference, state, mergedAt: pull.mergedAt, title: pull.title,
    mergeHandled: Boolean(previous?.mergeHandled || pull.merged), checkedAt: new Date().toISOString()};
  if (previous) Object.assign(previous, value); else task.references.push(value);
  if (!merged) return false;
  const source = activity(task, actor, `GitHub pull request merged: ${reference.url}\n${pull.title}`, 'github',
    {kind: 'event', githubEvent: 'pull_request', pullRequest: reference.url, mergedAt: pull.mergedAt});
  prompt(task, controllerActor, githubWakeup('pull_request', reference.url),
    'controller', {notificationKey: threadKey(reference), causedBy: [source.id]});
  return true;
}

export async function reconcileGithubPullRequests(store, lookup, login, onlyURL) {
  const cache = new Map(), ids = [];
  for (const task of await store.list()) {
    if (task.deleteRequested || task.completionRequested || terminal.has(task.phase) || task.desiredState === 'Cancelled') continue;
    for (const reference of reportedPullRequests(task)) {
      if (onlyURL && reference.url !== onlyURL || reference.mergeHandled || reference.unavailable) continue;
      if (task.references.some(value => sameForgeRepository(value, reference) && value.unavailable?.scope === 'repository')) continue;
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
        if (!(error.status >= 400 && error.status <= 599) && !['TimeoutError', 'AbortError', 'TypeError'].includes(error.name)) throw error;
        console.error(JSON.stringify({event: 'github_pr_reconcile_failed', taskId: task.id, repository: reference.repository, pullRequest: reference.id,
          ...(Number.isInteger(error.status) ? {status: error.status} : {})}));
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

export function reopenGithubTask(task, actor, url) {
  if (task.githubIssueState !== 'closed' || task.deleteRequested || task.desiredState === 'Cancelled') return false;
  task.githubIssueState = 'open'; task.completionRequested = false;
  task.desiredState = 'Active'; task.phase = task.active ? 'Running' : 'Pending'; task.reason = null;
  activity(task, actor, `GitHub issue reopened: ${url}`, 'controller', {kind: 'control'});
  return true;
}

export async function reconcileGithubIssues(store, lookup) {
  for (const task of await store.list()) {
    const reference = task.references?.find(r => isForgeReference(r) && r.kind === 'issue');
    if (!reference || reference.unavailable) continue;
    const closeable = !task.deleteRequested && task.desiredState !== 'Cancelled' && task.phase !== 'Completed' && !task.completionRequested;
    const reopenable = task.githubIssueState === 'closed' && !task.deleteRequested && task.desiredState !== 'Cancelled';
    if (!closeable && !reopenable) continue;
    try {
      const issue = await lookup(reference.url);
      if (closeable && issue.state === 'closed') {
        await store.mutate(task.id, `github-closed:${reference.repository}:${reference.id}:${issue.closedAt || 'closed'}`,
          current => closeGithubTask(current, controllerActor, reference.url));
        console.log(JSON.stringify({event: 'github_issue_reconciled', taskId: task.id,
          repository: reference.repository, issue: reference.id, state: 'closed'}));
      } else if (reopenable && issue.state !== 'closed') {
        await store.mutate(task.id, `github-reopened:${reference.repository}:${reference.id}:${issue.updatedAt || 'open'}`,
          current => { if (reopenGithubTask(current, controllerActor, reference.url))
            prompt(current, controllerActor, githubWakeup('issues', reference.url), 'controller'); });
        console.log(JSON.stringify({event: 'github_issue_reconciled', taskId: task.id,
          repository: reference.repository, issue: reference.id, state: 'open'}));
      }
    } catch (error) {
      if ([404, 410].includes(error.status)) {
        await store.mutate(task.id, 'github-unavailable:' + randomUUID(),
          current => unavailableGithubIssue(current, reference, error));
        console.log(JSON.stringify({event: 'github_issue_unavailable', taskId: task.id,
          repository: reference.repository, issue: reference.id, status: error.status,
          repositoryUnavailable: Boolean(error.repositoryUnavailable)}));
        continue;
      }
      // One inaccessible repository must not prevent other task/Pod reconciliation.
      console.error(JSON.stringify({event: 'github_issue_reconcile_failed', taskId: task.id,
        repository: reference.repository, issue: reference.id,
        ...(Number.isInteger(error.status) ? {status: error.status} : {})}));
    }
  }
}

export function githubTask(namespace, actor, issue, eventType, context, delivery) {
  const task = newTask(namespace, actor, issue.content, 'webhook', [issue.reference]);
  task.title = String(issue.title || issue.content.split('\n')[0]).slice(0, 200);
  task.repository = issue.repository;
  // The initial source event is context, not a second dispatchable prompt.
  const source = task.events[0]; source.kind = 'event'; source.content = context;
  source.externalURL = githubEventURL(issue.reference, issue.commentURL);
  source.deliveryId = delivery;
  task.pending = [];
  delete task.notificationSuppressUntil;
  prompt(task, controllerActor, githubWakeup(eventType, issue.reference.url), 'controller',
    {notificationKey: threadKey(issue.reference), causedBy: [source.id]});
  return task;
}
