import {isForgeReference} from './forge.js';
import {randomUUID, createHash, timingSafeEqual} from 'node:crypto';
import {agentActor} from './identity.js';
import {turnMetrics} from './metrics.js';
import {parseDocument} from 'yaml';

export const terminal = new Set(['Completed', 'Failed', 'Cancelled']);
export class Problem extends Error {
  constructor(status, message) { super(message); this.status = status; }
}
export function text(value, max = 16000) {
  if (typeof value !== 'string' || !value.trim() || value.length > max) {
    throw new Problem(400, `Expected nonempty text of at most ${max} characters`);
  }
  return value.trim();
}
export function id(value) {
  if (!/^[a-f0-9-]{36}$/.test(value ?? '')) throw new Problem(400, 'Invalid Task ID');
  return value;
}
export function digest(value) { return createHash('sha256').update(value).digest('hex'); }
export function normalizeControls(value = {}) {
  if (!value || typeof value !== 'object' || Array.isArray(value) || Object.keys(value).length > 32) throw new Problem(400, 'Expected at most 32 named context controls');
  return Object.fromEntries(Object.entries(value).map(([key, control]) => {
    if (!/^[a-z][a-z0-9_-]{0,63}$/.test(key) || !control || typeof control !== 'object' || Array.isArray(control)
      || control.enabled !== undefined && typeof control.enabled !== 'boolean') throw new Problem(400, 'Invalid context control');
    return [key, {label: text(control.label || key, 80), description: control.description ? text(control.description, 500) : '', enabled: control.enabled ?? true}];
  }));
}
export function controlsFromMarkdown(content) {
  const fenced = /^```agent-controls\s*\r?\n([\s\S]*?)\r?\n```/m.exec(content);
  const front = /^---\r?\n([\s\S]*?)\r?\n---(?:\r?\n|$)/.exec(content);
  if (!fenced && !front) return {};
  const document = parseDocument((fenced || front)[1]);
  if (document.errors.length || document.warnings.length) throw new Problem(400, 'Invalid agent-controls YAML');
  let value; try { value = document.toJS({maxAliasCount: 0}); } catch { throw new Problem(400, 'Context control YAML aliases are not supported'); }
  return normalizeControls(fenced ? value : value?.agentControls || {});
}
export function setControl(task, key, enabled, actor) {
  if (!Object.hasOwn(task.controls || {}, key) || typeof enabled !== 'boolean') throw new Problem(400, 'Unknown context control or invalid on/off value');
  if (task.deleteRequested) throw new Problem(409, 'Task is being deleted');
  task.controls[key].enabled = enabled;
  activity(task, actor, `${task.controls[key].label}: ${enabled ? 'on' : 'off'}`, 'webui', {kind: 'control', action: 'context_control'});
}
export function tokenMatches(actual, expected) {
  if (!actual || !expected) return false;
  return timingSafeEqual(Buffer.from(digest(actual)), Buffer.from(digest(expected)));
}
export const controllerActor = {type: 'controller', name: 'Codemowers agent controller'};
const notificationWindowMs = 3 * 60 * 1000;
export function promptsReady(task) {
  return task.pending.length > 0;
}
export function activity(task, actor, content, source, extra = {}) {
  const entry = {id: randomUUID(), sequence: task.sequence + 1, createdAt: new Date().toISOString(),
    actor, content, source, ...extra};
  task.sequence = entry.sequence;
  task.events.push(entry);
  // Full native output lives on the PVC. Bound the API conversation history.
  if (task.events.length > 1000) {
    const pinned = new Set([...task.pending, ...(task.active?.currentPrompts || task.active?.prompts || [])]);
    const removable = task.events.findIndex(e=>e.id!==entry.id && !pinned.has(e.id));
    if (removable >= 0) task.events.splice(removable,1);
  }
  task.updatedAt = entry.createdAt;
  return entry;
}
export function prompt(task, actor, content, source, extra = {}) {
  if (task.deleteRequested || task.completionRequested || task.desiredState === 'Cancelled' || task.phase === 'Completed') {
    throw new Problem(409, 'This Task no longer accepts prompts');
  }
  content = text(content);
  const automatic = Boolean(extra.automatic || actor.type === 'controller' && extra.notificationKey);
  if (actor.type === 'controller' && extra.notificationKey) {
    const queued = task.events.find(event => event.kind === 'prompt' && event.actor.type === 'controller'
      && event.notificationKey === extra.notificationKey && task.pending.includes(event.id));
    if (queued) {
      const retained = new Set(task.events.map(event => event.id));
      queued.causedBy = [...new Set([...(queued.causedBy || []), ...(extra.causedBy || [])])].filter(id => retained.has(id));
      queued.content = content;
      task.updatedAt = new Date().toISOString();
      return queued;
    }
  }
  if (automatic && Date.parse(task.notificationSuppressUntil || '') > Date.now()) return null;
  if (automatic) task.notificationSuppressUntil = new Date(Date.now() + notificationWindowMs).toISOString();
  const entry = activity(task, actor, content, source, {kind: 'prompt', ...extra, ...(automatic ? {automatic: true} : {})});
  task.pending.push(entry.id);
  if (!task.active && task.desiredState === 'Active') task.phase = 'Pending';
  return entry;
}
export function removeQueuedPrompt(task, promptId, actor) {
  const index = task.pending.indexOf(promptId);
  if (index < 0) throw new Problem(409, 'Prompt is no longer queued; it may already be running');
  const entry = task.events.find(e => e.id === promptId);
  task.pending.splice(index, 1);
  if (entry) { entry.queueDisposition = 'removed'; entry.removedAt = new Date().toISOString(); }
  if (!task.pending.length && !task.active && task.desiredState === 'Active') {
    task.phase = 'Waiting'; task.reason = 'AwaitingInput';
  }
  activity(task, actor, 'Removed queued prompt', 'webui', {kind: 'control', action: 'remove_prompt', causedBy: [promptId]});
}
export function newTask(namespace, actor, content, source = 'webui', references = []) {
  const now = new Date().toISOString();
  const task = {id: randomUUID(), namespace, title: text(content).slice(0, 100),
    objective: content, creator: actor, origin: source, createdAt: now, updatedAt: now,
    revision: 0, sequence: 0, phase: 'Pending', desiredState: 'Active',
    pending: [], events: [], references, controls: controlsFromMarkdown(content), session: null, active: null, lastExecution: null};
  prompt(task, actor, content, source, actor.type === 'external' && ['webhook', 'alertmanager'].includes(source) ? {automatic: true} : {});
  return task;
}
export function changeState(task, state, actor) {
  if (!['Active', 'Paused', 'Cancelled', 'Completed'].includes(state)) throw new Problem(400, 'Invalid state');
  if (terminal.has(task.phase) || task.completionRequested) throw new Problem(409, 'Task is terminal');
  if (state === 'Completed') {
    if (task.active) throw new Problem(409, 'Stop active execution before accepting completion');
    task.phase = 'Completed'; task.desiredState = 'Paused';
  } else {
    if (task.active && state !== 'Active') task.active.stopRequested = true;
    task.desiredState = state;
    if (state === 'Active') {
      task.blocked = null; task.reason = null;
      for (const reference of task.references || []) delete reference.unavailable;
    }
    task.phase = task.active ? (state === 'Active' ? 'Running' : 'Stopping')
      : (state === 'Active' ? (task.pending.length ? 'Pending' : 'Waiting') : state);
  }
  activity(task, actor, state === 'Completed' ? 'Accepted completion' : `Requested ${state}`, 'webui', {kind: 'control'});
}
export function stateRevision(state, revision) {
  // Stopping expresses current user intent, regardless of intervening progress.
  if (['Paused', 'Cancelled'].includes(state)) return undefined;
  if (!Number.isInteger(revision)) throw new Problem(400, 'Expected Task revision');
  return revision;
}
export function blockExecution(task, executionId, summary) {
  if (task.active?.id !== executionId || task.desiredState !== 'Active') throw new Problem(409, 'Execution is not active');
  const message = text(summary,8000);
  task.blocked = {summary:message,executionId,createdAt:new Date().toISOString()};
  task.phase = 'Waiting'; task.reason = 'AwaitingConfirmation';
  task.active.idleTimeoutSeconds ||= 3600;
  activity(task,agentActor(executionId),message,'controller',{kind:'blocked',executionId});
}
export function workerIdleTimeoutSeconds(task) {
  const gitConversation = task.references?.some(reference => isForgeReference(reference) && ['issue', 'pr'].includes(reference.kind));
  return task.origin === 'webui' || gitConversation || task.emailConversation ? 3600 : 0;
}
export function reserve(task) {
  if (task.active || task.desiredState !== 'Active' || terminal.has(task.phase) || !promptsReady(task)) return;
  const execution = {id: randomUUID(), sessionEpoch: task.session?.epoch ?? task.sessionEpoch ?? 1,
    startedAt: new Date().toISOString(), state: 'Preparing', prompts: task.pending.splice(0),
    token: randomUUID() + randomUUID(), sessionId: task.session?.id || randomUUID(), model: task.model};
  execution.currentPrompts = [...execution.prompts];
  execution.idleTimeoutSeconds = workerIdleTimeoutSeconds(task);
  execution.podName = `agent-worker-${task.id.slice(0, 8)}-${execution.id.slice(0, 8)}`;
  task.active = execution; task.blocked = null; task.phase = 'Running'; task.reason = null;
  activity(task, controllerActor, 'Starting worker', 'controller', {kind: 'execution', executionId: execution.id, podName: execution.podName});
  return execution;
}
export function interruptPrompt(task, executionId, promptIds, actor) {
  const active = task.active;
  if (task.desiredState !== 'Active' || active?.id !== executionId || active.state === 'Idle') throw new Problem(409, 'The displayed prompt is no longer running');
  if (!active.supportsPromptStop) throw new Problem(409, 'Restart this worker once to enable prompt-only stop');
  const current = active.currentPrompts || [];
  if (!Array.isArray(promptIds) || !promptIds.length || new Set(promptIds).size !== current.length || promptIds.length !== current.length || !promptIds.every(id => current.includes(id))) {
    throw new Problem(409, 'The displayed prompt is no longer running');
  }
  if (active.interrupt) return;
  active.interrupt = {promptIds: [...current], requestedAt: new Date().toISOString()};
  for (const event of task.events) if (current.includes(event.id)) event.queueDisposition = 'stopping';
  activity(task, actor, 'Stopping prompt', 'webui', {kind: 'control', action: 'interrupt_prompt', executionId});
}
export function finish(task, executionId, result) {
  if (task.active?.id !== executionId) throw new Problem(409, 'Stale execution');
  const execution = task.active;
  task.lastExecution = {...execution, token: undefined, state: result.exitCode === 0 ? 'Succeeded' : 'Failed',
    finishedAt: new Date().toISOString(), exitCode: result.exitCode, result};
  if (result.sessionId) task.session = {id: result.sessionId, epoch: execution.sessionEpoch, backend: execution.model?.backend || 'claude'};
  if (!execution.lastTurnResult || execution.lastTurnResult.summary !== result.summary) recordResult(task, executionId, result);
  task.active = null;
  // Interrupted inputs remain deliverable when the user resumes, including a
  // pause before the CLI stored its first conversation turn.
  if (execution.stopRequested && task.desiredState !== 'Cancelled' && !task.completionRequested) {
    task.pending = [...new Set([...execution.prompts.filter(p => !execution.completedPrompts?.includes(p)
      && !task.events.some(e => e.id === p && e.queueDisposition === 'removed')), ...task.pending])];
  }
  // Desired state always wins over late process success.
  if (task.completionRequested && !task.deleteRequested && task.desiredState !== 'Cancelled') task.phase = 'Completed';
  else if (task.desiredState !== 'Active') task.phase = task.desiredState;
  else if (task.pending.length) task.phase = 'Pending';
  else { task.phase = 'Waiting'; task.reason = task.blocked ? 'AwaitingConfirmation' : result.exitCode === 0 ? 'AwaitingReview' : 'ExecutionFailed'; }
  return task;
}
export function recordResult(task, executionId, result) {
  const summary = (result.summary || 'Execution ended without a final answer').slice(0, 64000);
  const metrics = result.metrics || turnMetrics(undefined, result.usage);
  const previous = task.events.findLast(e => e.executionId === executionId && e.kind === 'progress');
  if (previous && previous.content === summary.slice(0, 8000)) {
    previous.kind = 'result'; previous.content = summary; previous.metrics = metrics;
  } else activity(task, agentActor(executionId), summary, 'controller', {kind: 'result', executionId, metrics});
}
export function workerEvent(task, executionId, entry) {
  if (task.active?.id !== executionId) return;
  const active = task.active;
  if (Number.isSafeInteger(entry.sequence)) {
    if (entry.sequence <= (active.logSequence || 0)) return;
    active.logSequence = entry.sequence;
  }
  if (entry.kind === 'tool' && typeof entry.toolId === 'string') {
    const previous = task.events.findLast(e=>e.kind==='tool' && e.executionId===executionId && e.toolId===entry.toolId);
    const fields = {toolId:entry.toolId.slice(0,200),toolState:entry.toolState,
      ...(entry.name?{toolName:String(entry.name).slice(0,200)}:{}),
      ...(entry.detail?{toolDetail:String(entry.detail).slice(0,1000)}:{}),
      ...(entry.command?{command:String(entry.command).slice(0,8100)}:{}),
      ...(Number.isInteger(entry.exitCode)?{exitCode:entry.exitCode}:{})};
    if (entry.mcpRequest !== undefined) fields.mcpRequest = entry.mcpRequest;
    if (entry.mcpResponse !== undefined) fields.mcpResponse = entry.mcpResponse;
    if (previous) Object.assign(previous,fields,{updatedAt:new Date().toISOString()});
    else activity(task,agentActor(executionId),'', 'controller',{kind:'tool',executionId,...fields});
    return;
  }
  if (entry.kind === 'turn_started') { active.turnStartedAt = entry.startedAt; active.supportsPromptStop = entry.supportsPromptStop === true; }
  if (entry.kind === 'session_reset') {
    active.sessionEpoch++;
    activity(task, controllerActor, entry.reason, 'controller', {kind: 'recovery', executionId});
  }
  if (entry.kind === 'session' || entry.kind === 'session_reset') {
    task.session = {id: entry.sessionId, backend: active.model?.backend || 'claude', epoch: active.sessionEpoch};
  }
  if (entry.kind === 'result') active.result = entry.result;
  if (entry.kind === 'turn_result') {
    if (entry.result.interrupted) {
      for (const event of task.events) if (active.currentPrompts?.includes(event.id)) event.queueDisposition = 'stopped';
      if (!active.idleTimeoutSeconds) active.idleTimeoutSeconds = 3600;
    }
    delete active.interrupt;
    active.lastTurnSequence = entry.sequence;
    active.result = entry.result; active.lastTurnResult = entry.result;
    active.completedPrompts = [...new Set([...(active.completedPrompts || []), ...(active.currentPrompts || active.prompts)])];
    active.currentPrompts = []; active.state = 'Idle';
    const finished = Date.parse(entry.result.completedAt);
    const completedAt = Number.isFinite(finished) ? Math.min(finished, Date.now()) : Date.now();
    active.idleExpiresAt = new Date(completedAt + (active.idleTimeoutSeconds || 0) * 1000).toISOString();
    if (task.desiredState === 'Active') { task.phase = 'Waiting'; task.reason = task.blocked ? 'AwaitingConfirmation' : entry.result.interrupted ? 'PromptStopped' : entry.result.exitCode === 0 ? 'AwaitingInput' : 'ExecutionFailed'; }
    recordResult(task, executionId, entry.result);
  }
  if (entry.summary) activity(task, agentActor(executionId), entry.summary.slice(0, 8000),
    'controller', {kind: 'progress', executionId});
}
export function claimNextTurn(task, executionId) {
  if (task.active?.id !== executionId || task.desiredState !== 'Active' || task.active.state !== 'Idle' || !promptsReady(task)) return;
  task.active.currentPrompts = task.pending.splice(0);
  task.active.prompts.push(...task.active.currentPrompts);
  task.active.result = null; task.active.state = 'Running'; task.active.idleExpiresAt = null;
  delete task.active.interrupt;
  task.active.turnStartedAt = null;
  task.phase = 'Running'; task.reason = null; task.blocked = null;
}
export function renameTask(task, title, actor) {
  if (task.deleteRequested) throw new Problem(409, 'Task is being deleted');
  task.title = text(title, 200);
  activity(task, actor, `Renamed task to ${task.title}`, 'webui', {kind: 'control'});
}
export function deleteTask(task, actor) {
  if (task.deleteRequested) return;
  task.deleteRequested = true; task.desiredState = 'Cancelled';
  task.phase = task.active ? 'Stopping' : 'Cancelled';
  if (task.active) task.active.stopRequested = true;
  activity(task, actor, 'Requested deletion of task and scratch workspace', 'webui', {kind: 'control'});
}
export function publicTask(task, detail = true) {
  const copy = structuredClone(task);
  for (const event of copy.events || []) {
    if (event.mcpRequest !== undefined || event.mcpResponse !== undefined) event.mcpPayloadAvailable = true;
    delete event.mcpRequest; delete event.mcpResponse;
  }
  delete copy.creationRequestHash;
  delete copy.shareEpoch;
  delete copy.notificationSuppressUntil;
  if (copy.draft) delete copy.draft.content;
  if (copy.active) delete copy.active.token;
  if (!detail) {
    delete copy.events;
  }
  return copy;
}
