import {digest, activity, prompt, controllerActor} from './domain.js';
import {githubTask, closeGithubTask, githubWakeup} from './github.js';

// A notification identifies a repository/issue, but is never authority for the
// assignment or instructions. Only the authenticated GitHub API supplies those.
export function githubMessageReference(value) {
  const match = /^<([a-z0-9_-]+)\/([a-z0-9_.-]+)(?:\/([^<>\s@]+))?@github\.com>$/i.exec(value || '');
  if (!match || ['.', '..'].includes(match[2]) || match[3]?.length > 500) return null;
  const repository = match[1] + '/' + match[2], parts = (match[3] || '').split('/');
  let kind = 'issue', number;
  if (/^(issue|issues|pull|pulls)$/.test(parts[0])) {
    kind = /^pull/.test(parts[0]) ? 'pr' : 'issue'; number = parts[1];
  } else if (/^[1-9]\d*$/.test(parts[0])) number = parts[0];
  if (!/^[1-9]\d*$/.test(number || '')) return {repository};
  return {repository, kind, id: number,
    url: `https://github.com/${repository}/${kind === 'pr' ? 'pull' : 'issues'}/${number}`};
}

export function githubNotificationReference(mail) {
  const references = Array.isArray(mail.references) ? mail.references : (mail.references || '').match(/<[^<>\s]+>/g) || [];
  const parsed = [mail.messageId, mail.inReplyTo, ...references.slice().reverse()].map(githubMessageReference).filter(Boolean);
  return parsed.find(value => value.id) || parsed[0] || null;
}

export async function resolveGithubMailIssues(reference, login, lookup, listAssigned) {
  if (reference.id) {
    try { return [await lookup(reference.url)]; }
    catch (error) { if (error.status !== 404) throw error; }
  }
  return listAssigned(reference.repository, login);
}

export async function routeGithubIssueMail(store, namespace, issue, login, mailId, options = {}) {
  const key = `github:${issue.repository}:${issue.reference.id}`;
  const mapping = store.prefix + 'external:' + digest(key), mapped = await store.redis.get(mapping);
  const actor = {type: 'external', provider: 'github', name: 'GitHub notification'};
  if (options.backfill && mapped) return {outcome: 'duplicate', taskId: mapped};
  if (issue.isPullRequest) return {outcome: 'ignored', reason: 'pull_request'};
  if (issue.state === 'closed') {
    if (!mapped) return {outcome: 'ignored', reason: 'closed_without_task'};
    await store.mutate(mapped, 'github-mail-closed:' + digest(mailId), current => closeGithubTask(current, actor, issue.reference.url));
    return {outcome: 'completed', taskId: mapped};
  }
  if (!issue.assignees?.some(value => value.toLowerCase() === login.toLowerCase())) return {outcome: 'ignored', reason: 'not_assigned_to_agent'};
  const delivery = 'github-mail:' + digest(mailId);
  issue = {...issue, content: String(issue.content).slice(0, 15000)};
  const context = `GitHub notification for ${issue.reference.url}. The GitHub API confirms this open issue is assigned to ${login}.\n\n${issue.content}`;
  const proposed = githubTask(namespace, actor, issue, 'issues', context, delivery);
  proposed.githubIssueUpdatedAt = issue.updatedAt;
  const task = await store.create(proposed, 'external:' + key);
  await store.redis.set(mapping, task.id);
  if (task.id === proposed.id) return {outcome: 'created', taskId: task.id};
  if (options.backfill) return {outcome: 'duplicate', taskId: task.id};
  let outcome = 'duplicate';
  await store.mutate(task.id, delivery, current => {
    if (current.githubIssueState === 'closed' && !current.deleteRequested && current.desiredState !== 'Cancelled') {
      current.githubIssueState = 'open'; current.completionRequested = false;
      current.desiredState = 'Active'; current.phase = current.active ? 'Running' : 'Pending'; current.reason = null;
    }
    if (current.deleteRequested || current.completionRequested || current.phase === 'Completed' || current.desiredState === 'Cancelled') return;
    if (current.events.some(e => e.deliveryId === delivery) || issue.updatedAt && current.githubIssueUpdatedAt === issue.updatedAt) return;
    const source = activity(current, actor, context, 'email', {kind: 'event', deliveryId: delivery, githubIssue: issue.reference.url});
    prompt(current, controllerActor, githubWakeup('email', issue.reference.url),
      'controller', {causedBy: [source.id]});
    current.githubIssueUpdatedAt = issue.updatedAt; outcome = current.desiredState === 'Paused' ? 'queued' : 'resumed';
  });
  return {outcome, taskId: task.id};
}
