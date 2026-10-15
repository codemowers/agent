import {randomBytes, randomUUID, createHmac, timingSafeEqual} from 'node:crypto';
import {githubUsername} from './profiles.js';
import {Problem} from './domain.js';

const pick = (value, keys) => Object.fromEntries(keys.filter(key => value?.[key] !== undefined).map(key => [key, value[key]]));
const actor = value => {
  const login = githubUsername(value?.type === 'user' ? {type: 'user', githubUsername: value.githubUsername} : value);
  return {...pick(value, ['type', 'name', 'avatarHash']), ...(login ? {githubUsername: login} : {})};
};

export function sharedTask(task) {
  const result = pick(task, ['id', 'title', 'repository', 'phase', 'desiredState', 'reason', 'createdAt', 'updatedAt', 'pending']);
  result.creator = actor(task.creator);
  result.model = pick(task.model, ['backend', 'label']);
  result.controls = task.controls || {};
  if (task.draft) result.draft = pick(task.draft, ['id', 'title', 'digest']);
  result.references = (task.references || []).map(reference => pick(reference, ['source', 'kind', 'id', 'repository']));
  result.active = task.active ? pick(task.active, ['state', 'podName', 'podReady', 'currentPrompts', 'prompts', 'turnStartedAt', 'idleExpiresAt', 'idleTimeoutSeconds']) : null;
  if (task.lastExecution) result.lastExecution = pick(task.lastExecution, ['podName']);
  if (task.blocked) result.blocked = pick(task.blocked, ['summary']);
  result.events = task.events.map(event => ({...pick(event, ['id', 'kind', 'action', 'content', 'createdAt', 'updatedAt',
    'executionId', 'podName', 'causedBy', 'automatic', 'queueDisposition', 'toolName', 'toolDetail', 'toolState', 'command', 'exitCode']), actor: actor(event.actor),
    ...(event.metrics ? {metrics: pick(event.metrics, ['durationMs', 'apiDurationMs', 'inputTokens', 'outputTokens', 'cacheReadTokens', 'cacheWriteTokens'])} : {})}));
  return result;
}

export class SharedLinks {
  constructor(store, {mode = 'disabled', key} = {}) {
    if (!['public', 'signed', 'disabled'].includes(mode)) throw new Error('Invalid AGENT_SHARING_MODE');
    if (mode !== 'disabled' && (typeof key !== 'string' || key.length < 32)) throw new Error('Sharing signing key must contain at least 32 characters');
    this.store = store; this.mode = mode; this.key = key;
  }
  async create(taskId) {
    if (this.mode === 'disabled') throw new Problem(403, 'Sharing is disabled');
    const task = await this.store.mutate(taskId, 'share:' + randomUUID(), task => {
      if (task.deleteRequested) throw new Problem(404, 'Task not found');
      task.shared = true;
    });
    const payload = Buffer.from(JSON.stringify({taskId, scope: this.store.prefix, epoch: task.shareEpoch || 0,
      nonce: randomBytes(16).toString('base64url')})).toString('base64url');
    const token = payload + '.' + createHmac('sha256', this.key).update(payload).digest('base64url');
    return {token};
  }
  published(task) { return !task.deleteRequested && task.shared === true; }
  async list() {
    if (this.mode !== 'public') throw new Problem(404, 'Public sharing is disabled');
    return (await this.store.list()).filter(task => this.published(task)).map(task => {
      const copy = sharedTask(task); delete copy.events; return copy;
    }).sort((a, b) => b.updatedAt.localeCompare(a.updatedAt));
  }
  async publicRead(taskId) {
    if (this.mode !== 'public') throw new Problem(404, 'Public sharing is disabled');
    const task = await this.store.get(taskId);
    if (!this.published(task)) throw new Problem(404, 'Share link unavailable');
    return {task: sharedTask(task)};
  }
  async read(token) {
    const missing = () => new Problem(404, 'Share link unavailable');
    if (this.mode === 'disabled' || typeof token !== 'string' || token.length > 2048
      || !/^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]{43}$/.test(token)) throw missing();
    const [payload, signature] = token.split('.');
    const expected = createHmac('sha256', this.key).update(payload).digest('base64url');
    if (!timingSafeEqual(Buffer.from(signature), Buffer.from(expected))) throw missing();
    let link;
    try { link = JSON.parse(Buffer.from(payload, 'base64url').toString('utf8')); } catch { throw missing(); }
    if (!link || typeof link.taskId !== 'string' || !Number.isSafeInteger(link.epoch) || link.epoch < 0
      || typeof link.nonce !== 'string' || link.scope !== this.store.prefix) throw missing();
    let task;
    try { task = await this.store.get(link.taskId); }
    catch (error) { if (error.status === 404) throw missing(); throw error; }
    if (!this.published(task) || (task.shareEpoch || 0) !== link.epoch) throw missing();
    return {task: sharedTask(task)};
  }
}
