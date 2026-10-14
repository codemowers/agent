import {UserManager, WebStorageStateStore, InMemoryWebStorage} from 'oidc-client-ts';
import {groupAgentMessages} from './conversation.js';
import {markdown} from './markdown.js';
import {responseJson} from './http.js';
import {refinementPrompt, refinementRepository, transcriptText} from '../src/refinement-defaults.js';
import {githubUsername} from '../src/profiles.js';
import {installSidebarResize} from './sidebar-resize.js';
import {rotateTaglines} from './taglines.js';

const $ = selector => document.querySelector(selector);
const mobileSidebar = matchMedia('(max-width: 900px)');
function placeConnectivity() {
  $(mobileSidebar.matches ? '#sidebar-connectivity' : '#navbar-connectivity').append($('#connectivity-navbar'));
}
function setSidebar(open, restoreFocus = false) {
  open = mobileSidebar.matches && open;
  document.body.classList.toggle('sidebar-open', open);
  $('#sidebar-toggle').setAttribute('aria-expanded', String(open));
  $('#sidebar-toggle').setAttribute('aria-label', open ? 'Close task menu' : 'Open task menu');
  $('#sidebar-backdrop').hidden = !open;
  $('#task-sidebar').inert = mobileSidebar.matches && !open;
  $('#welcome').inert = open; $('#detail').inert = open;
  if (open) $('#sidebar-close').focus();
  else if (restoreFocus) $('#sidebar-toggle').focus();
}
$('#sidebar-toggle').onclick = () => setSidebar(!document.body.classList.contains('sidebar-open'), true);
$('#sidebar-close').onclick = $('#sidebar-backdrop').onclick = () => setSidebar(false, true);
mobileSidebar.addEventListener('change', () => { setSidebar(false); placeConnectivity(); });
document.addEventListener('keydown', event => {
  if (!document.body.classList.contains('sidebar-open')) return;
  if (event.key === 'Escape') { event.preventDefault(); setSidebar(false, true); }
  if (event.key === 'Tab') {
    const buttons = [...$('#task-sidebar').querySelectorAll('button:not(:disabled),a[href],[tabindex="0"]')].filter(button => button.offsetParent !== null);
    const first = buttons[0], last = buttons.at(-1);
    if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
    else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
  }
});
for (const selector of ['#tasks', '#new', '#browse-drafts', '#instructions', '#connections']) $(selector).addEventListener('click', () => setSidebar(false));
setSidebar(false);
placeConnectivity();
installSidebarResize($('#sidebar-resize'), $('#task-sidebar'), mobileSidebar);
rotateTaglines($('#rotating-tagline'));
const cfg = await fetch('/config').then(r => r.json());
document.title = `${cfg.identity?.name || 'Coding agent'} · codemowers/agent`;
const auth = new UserManager({authority: cfg.issuer, client_id: cfg.clientId,
  redirect_uri: cfg.redirectURI, response_type: 'code', scope: cfg.scopes,
  extraQueryParams: {resource: cfg.audience}, extraTokenParams: {resource: cfg.audience},
  userStore: new WebStorageStateStore({store: new InMemoryWebStorage()}),
  stateStore: new WebStorageStateStore({store: sessionStorage}),
  automaticSilentRenew: false, loadUserInfo: false});
let user, currentActor, selected, tasks = [], modelOptions = [], modelDirty = false, sending = false, selectedRepository = null, setupData;
const shareToken = new URLSearchParams(location.hash.slice(1)).get('share');
const sharedTaskId = new URLSearchParams(location.search).get('sharedTask');
let readOnly = Boolean(shareToken || sharedTaskId);
let shareTaskId;
let publicDirectory = false;
let selectedDraft = null;
let selectedRefinement = null;
let composerStep = 'choose';
const expandedDetails = new Map();
if (readOnly) document.body.classList.add('read-only');
function populateModels(selector, backend, current) {
  const select = $(selector), wanted = current || select.value;
  select.replaceChildren();
  for (const model of modelOptions.filter(m => m.backends.includes(backend))) {
    const option = document.createElement('option'); option.value = model.id; option.textContent = model.label;
    select.append(option);
  }
  if ([...select.options].some(o => o.value === wanted)) select.value = wanted;
  else select.value = backend === 'codex' ? 'openai:default' : 'anthropic:default';
  select.disabled = !user || !select.options.length;
}
async function refreshModels() {
  const catalog = await api('/models'); modelOptions = catalog.models;
  populateModels('#model', $('#backend').value);
}
$('#backend').onchange = () => populateModels('#model', $('#backend').value);
$('#task-backend').onchange = () => { modelDirty = true; populateModels('#task-model', $('#task-backend').value); };
$('#task-model').onchange = () => { modelDirty = true; };
function failure(error) { $('#error').hidden = false; $('#error').textContent = error.message; $('#error').dataset.transient = String(Boolean(error.transient)); }
async function api(path, method = 'GET', body) {
  if (readOnly) throw new Error('This shared task is read-only.');
  if (!user || user.expired) { $('#connectivity-navbar').hidden = true; throw new Error('Session expired. Sign in again.'); }
  const response = await fetch('/api' + path, {method, headers: {Authorization: `Bearer ${user.access_token}`,
    'Content-Type': 'application/json', ...(method !== 'GET' ? {'Idempotency-Key': crypto.randomUUID()} : {})},
    ...(body ? {body: JSON.stringify(body)} : {})});
  if (response.status === 401) $('#connectivity-navbar').hidden = true;
  const result = await responseJson(response);
  if ($('#error').dataset.transient === 'true') $('#error').hidden = true;
  return result;
}
function node(tag, content, className) {
  const n = document.createElement(tag); n.textContent = content;
  if (className) n.className = className;
  return n;
}
function chooseRepository(repository) {
  if (selectedRefinement && repository !== refinementRepository) return;
  selectedRepository = repository;
  $('#repository-label').textContent = repository || 'Fresh workspace · no repository selected';
  $('#clear-repository').hidden = !repository || Boolean(selectedRefinement);
  for (const button of $('#repository-list').querySelectorAll('[data-repository]')) {
    button.classList.toggle('selected', button.dataset.repository === repository);
    button.setAttribute('aria-pressed', String(button.dataset.repository === repository));
  }
}
$('#clear-repository').onclick = () => chooseRepository(null);
function renderResources() {
  const build = setupData.infrastructure;
  $('#build-status').textContent = build?.detail || 'Build configuration discovery unavailable.';
  const buildConfiguration = $('#build-configuration'); buildConfiguration.replaceChildren();
  if (build?.status === 'available') {
    buildConfiguration.append(node('div', 'Default repository: ' + build.skaffold.defaultRepo),
      node('small', `Kaniko · namespace ${build.namespace} · Secret ${build.pushSecret.name} / ${build.pushSecret.key}`));
  }
  $('#new-task-resources').hidden = false;
  const github = setupData.github;
  $('#github-identity').textContent = github ? `GitHub · ${github.login}` : 'GitHub unavailable';
  $('#resource-warning').textContent = (setupData.warnings || []).join(' · ');
  const repositories = $('#repository-list'); repositories.replaceChildren();
  for (const repo of github?.repositories || []) {
    const row = node('div', '', 'resource-row');
    const button = node('button', repo.name, 'repository-choice'); button.type = 'button'; button.title = repo.name;
    button.dataset.repository = repo.name; button.disabled = repo.archived || Boolean(selectedRefinement && repo.name !== refinementRepository);
    button.onclick = () => { chooseRepository(repo.name); $('#objective').focus(); };
    const link = node('a', '↗', 'external-link'); link.href = repo.url; link.target = '_blank'; link.rel = 'noopener noreferrer';
    link.title = 'Open ' + repo.name + ' on GitHub'; link.setAttribute('aria-label', link.title);
    row.append(button, node('small', repo.archived ? 'Archived' : repo.writable ? 'Read/write' : 'Read only'), link); repositories.append(row);
  }
  if (!github?.repositories.length) repositories.append(node('p', 'No accessible repositories found.', 'empty-resource'));
  $('#repository-count').textContent = github ? `Showing ${github.repositories.length} of ${github.clipped ? 'at least ' : ''}${github.repositoryCount} repositories` : '';
  const destinations = $('#creation-destinations'); destinations.replaceChildren();
  for (const destination of github?.destinations || []) {
    const row = node('div', '', 'destination-row');
    const link = node('a', destination.owner); link.href = `https://github.com/new?owner=${encodeURIComponent(destination.owner)}`;
    link.target = '_blank'; link.rel = 'noopener noreferrer';
    const status = destination.policy === 'restricted' ? 'Organization policy restricts creation' : destination.policy === 'unknown'
      ? 'Creation policy unknown' : destination.tokenPermission === 'confirmed' ? `Creation allowed${destination.visibility ? ' · ' + destination.visibility : ''}` : 'Policy allows creation · token permission unknown';
    row.append(link, node('small', `${destination.kind === 'user' ? 'Personal account' : 'Organization'} · ${status}`)); destinations.append(row);
  }
  chooseRepository(selectedRepository);
}
async function refreshResources(force = false) {
  const button = $('#refresh-resources'); button.disabled = true;
  try { setupData = await api('/setup' + (force ? '?refresh=true' : '')); renderResources(); }
  catch (error) { $('#new-task-resources').hidden = false; $('#resource-warning').textContent = error.message; }
  finally { button.disabled = false; }
}
const connectionNames = ['codemowers', 'driftmower', 'agent', 'github', 'inbound-mail', 'outbound-mail', 'registry'];
function renderConnectivity(connections = []) {
  for (const name of connectionNames) {
    const badge = $('#connection-' + name), status = connections.find(connection => connection.name === name);
    const online = status?.status === 'available';
    badge.classList.toggle('online', online); badge.classList.toggle('offline', !online);
    badge.querySelector('.state-dot').className = 'state-dot ' + (online ? 'green' : 'red');
    const mail = ['inbound-mail', 'outbound-mail'].includes(name);
    badge.title = `${status?.source === 'operation' ? 'Last operation' : 'Startup check'} ${online ? 'passed' : 'failed'} · ${status?.detail || 'Not configured or unavailable'}\nLast check: ${status?.checkedAt ? new Date(status.checkedAt).toLocaleString() : 'Unavailable'}\n${mail ? 'Updated by normal IMAP/SMTP activity; no additional probes.' : 'Checked once per controller startup; not continuously monitored.'}`;
    if (status?.probeReference) badge.title += '\nProbe image: ' + status.probeReference;
    badge.setAttribute('aria-label', badge.textContent.trim() + ' · ' + (online ? 'check passed' : 'check failed'));
    if (['codemowers', 'driftmower', 'registry'].includes(name)) {
      badge.removeAttribute('href');
      try {
        const url = new URL(status?.url);
        if (['https:', 'http:'].includes(url.protocol)) badge.href = url.origin + '/';
      } catch { /* Missing or invalid configuration has no external link. */ }
    }
  }
}
async function loadConnectivity() {
  try {
    const status = await api('/connectivity');
    renderConnectivity(status.connections);
  } catch {
    renderConnectivity(connectionNames.map(name => ({name, status: 'unavailable', detail: 'Unable to load startup check results; reload the page to try again'})));
  }
}
async function refreshDrafts() {
  const drafts = await fetch('/drafts').then(responseJson), gallery = $('#draft-gallery'); gallery.replaceChildren();
  const scratch = node('button', 'Start from scratch', 'draft-choice scratch-choice'); scratch.type = 'button';
  scratch.append(node('small', 'Describe something new without a preset specification.'));
  scratch.onclick = () => openComposer('scratch'); gallery.append(scratch);
  for (const draft of drafts) {
    const button = node('button', draft.title, 'draft-choice'); button.type = 'button';
    button.title = `${draft.id} · ${Math.ceil(draft.size / 1024)} KiB`;
    button.onclick = () => useDraft(draft.id).catch(failure);
    gallery.append(button);
  }
  $('#drafts-section').hidden = composerStep === 'edit';
}
async function useDraft(id) {
  const draft = await fetch('/drafts/' + encodeURIComponent(id)).then(responseJson);
  openComposer('scratch'); selectedDraft = draft;
  $('#draft-selection').textContent = 'Reference draft: ' + selectedDraft.title;
  $('#clear-draft').hidden = false;
  $('#objective').value = 'Implement the attached draft: ' + selectedDraft.title + '.';
  $('#reference-content').value = selectedDraft.content;
  $('#reference-editor').hidden = false;
  $('#task-composer-title').textContent = selectedDraft.title;
  $('#reference-content').focus();
}
$('#clear-draft').onclick = () => {
  selectedDraft = null; selectedRefinement = null;
  $('#draft-selection').textContent = ''; $('#clear-draft').hidden = true;
  $('#reference-editor').hidden = true; $('#reference-content').value = '';
  $('#clear-repository').hidden = !selectedRepository;
};
$('#refresh-resources').onclick = () => refreshResources(true);
const avatarHashes = new Map();
function avatar(actor) {
  const image = document.createElement('img'); image.className = 'avatar'; image.alt = '';
  image.width = 28; image.height = 28; image.loading = 'lazy'; image.referrerPolicy = 'no-referrer';
  const fallback = () => {
    const hash = (actor.type === 'agent' ? cfg.identity?.avatarHash : undefined) || actor.avatarHash || (actor.type === 'user' && actor.issuer === currentActor?.issuer && actor.subject === currentActor?.subject ? currentActor?.avatarHash : undefined);
    if (/^[a-f0-9]{64}$/.test(hash || '')) {
      image.src = `https://www.gravatar.com/avatar/${hash}?s=56&d=identicon`; return;
    }
    const identity = actor.type === 'agent' ? 'codemowers/agent' : actor.type === 'controller' ? 'codemowers/controller'
      : `${actor.issuer || actor.provider || actor.type}:${actor.subject || actor.name}`;
    if (!avatarHashes.has(identity)) avatarHashes.set(identity, crypto.subtle.digest('SHA-256', new TextEncoder().encode(identity))
      .then(bytes => [...new Uint8Array(bytes)].map(byte => byte.toString(16).padStart(2, '0')).join('')));
    avatarHashes.get(identity).then(hash => { image.src = `https://www.gravatar.com/avatar/${hash}?s=56&d=${actor.type === 'agent' ? 'robohash' : 'identicon'}&f=y`; });
  };
  const login = githubUsername(actor, cfg.identity);
  if (login) {
    image.onerror = () => { image.onerror = null; fallback(); };
    image.src = `https://github.com/${encodeURIComponent(login)}.png?size=56`;
  } else fallback();
  return image;
}
function authorLink(actor, label) {
  const login = githubUsername(actor, cfg.identity);
  const element = node(login ? 'a' : 'span', label, 'author');
  if (login) {
    element.href = `https://github.com/${encodeURIComponent(login)}`;
    element.target = '_blank'; element.rel = 'noopener noreferrer';
  }
  return element;
}
function duration(ms) {
  const seconds = Math.max(0, Math.round(ms / 1000));
  return seconds < 60 ? `${seconds}s` : `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
}
function responseMetrics(metrics) {
  const parts = [];
  if (Number.isFinite(metrics.durationMs)) parts.push(`${duration(metrics.durationMs)} elapsed`);
  if (Number.isFinite(metrics.apiDurationMs)) parts.push(`${duration(metrics.apiDurationMs)} in API calls`);
  if (Number.isFinite(metrics.inputTokens)) parts.push(`${metrics.inputTokens.toLocaleString()} input tokens`);
  if (Number.isFinite(metrics.outputTokens)) parts.push(`${metrics.outputTokens.toLocaleString()} output tokens`);
  if (metrics.cacheReadTokens) parts.push(`${metrics.cacheReadTokens.toLocaleString()} cached tokens`);
  if (metrics.cacheWriteTokens) parts.push(`${metrics.cacheWriteTokens.toLocaleString()} cache-write tokens`);
  if (metrics.durationMs > 0 && metrics.outputTokens > 0) parts.push(`${(metrics.outputTokens / (metrics.durationMs / 1000)).toFixed(1)} output tokens/s · whole-turn average`);
  return parts.join(' · ');
}
function renderList() {
  const list = $('#tasks'); list.replaceChildren();
  for (const task of tasks) {
    const row = node('div', '', 'task' + (task.id === selected?.id ? ' selected' : ''));
    const button = node('button', '', 'task-select'); button.type = 'button';
    const status = workerStatus(task), heading = node('div', '', 'task-heading');
    const dot = node('i', '', 'state-dot ' + status.color); dot.title = status.worker || status.label;
    dot.setAttribute('aria-label', dot.title);
    const reference = task.references?.find(r => r.source === 'github' && ['issue', 'pr', 'pull_request'].includes(r.kind));
    const repository = reference?.repository || task.repository;
    const label = `${repository ? `${repository}${reference?.id ? ` #${reference.id}` : ''} ` : ''}${task.title}`;
    button.title = label;
    heading.append(dot, node('strong', label));
    button.append(heading);
    const meta = node('div', '', 'task-meta');
    meta.append(node('span', status.label + ' · '), authorLink(task.creator, task.creator.name || task.creator.type));
    row.append(button, meta);
    button.onclick = () => publicDirectory || (!user && cfg.sharingMode === 'public')
      ? location.assign('/?sharedTask=' + encodeURIComponent(task.id)) : select(task.id).catch(failure);
    row.onclick = event => { if (!event.target.closest('button, a')) button.click(); };
    list.append(row);
  }
}
function confirmationLabel(task) {
  return task.references?.some(reference => reference.source === 'github' && reference.kind === 'issue')
    ? 'Awaiting confirmation on GitHub' : 'Awaiting confirmation';
}
function workerStatus(task) {
  const pod = task.active?.podName || task.lastExecution?.podName || 'Worker';
  if (!task.active) return {color: task.reason === 'ExecutionFailed' && task.phase !== 'Paused' ? 'red' : 'gray', worker: `${pod} stopped`,
    label: task.phase === 'Completed' ? 'Completed' : task.phase === 'Cancelled' ? 'Cancelled' : task.phase === 'Paused' ? 'Paused · worker stopped'
      : task.reason === 'ExecutionFailed' ? 'Response failed' : task.phase === 'Pending' ? 'Waiting for worker'
      : task.reason === 'AwaitingConfirmation' ? confirmationLabel(task) : task.reason === 'AwaitingReview' ? 'Awaiting review' : 'Waiting for prompt'};
  if (task.desiredState !== 'Active') return {color: 'amber', worker: `${pod} stopping`,
    label: task.desiredState === 'Cancelled' ? 'Cancelling · worker stopping' : task.completionRequested ? 'Completing · worker stopping' : 'Pausing · worker stopping'};
  if (task.reason === 'AdmissionBlocked') return {color: 'red', label: 'Worker start blocked', worker: `${pod} start blocked by admission`};
  if (task.active.podReady === true || (task.active.podReady === undefined && ['Running', 'Idle'].includes(task.active.state))) {
    return {color: task.active.state === 'Idle' && task.blocked ? 'amber' : task.active.state === 'Idle' && task.reason === 'ExecutionFailed' ? 'red' : 'green',
      label: task.active.state === 'Idle' ? task.blocked ? confirmationLabel(task) : task.reason === 'ExecutionFailed' ? 'Response failed' : 'Awaiting input' : 'Processing prompt', worker: `${pod} running`};
  }
  return {color: 'amber', label: 'Starting worker', worker: `${pod} starting`};
}
function renderCountdown() {
  if (!selected) return;
  const active = selected.active;
  $('#turn-elapsed').textContent = active?.turnStartedAt && active.state !== 'Idle'
    ? `Current turn: ${duration(Date.now() - Date.parse(active.turnStartedAt))} elapsed` : '';
  let label = workerStatus(selected).label;
  if (active?.state === 'Idle' && active.idleExpiresAt && selected.desiredState === 'Active') {
    const remaining = Math.max(0, Math.ceil((Date.parse(active.idleExpiresAt) - Date.now()) / 1000));
    label += remaining ? ` · idle shutdown in ${Math.floor(remaining / 60)}:${String(remaining % 60).padStart(2, '0')}` : ' · shutting down';
  }
  $('#phase').textContent = label;
  $('#phase').title = `${selected.model?.backend || 'claude'} · ${selected.model?.label || 'Default model'}\n${workerStatus(selected).worker}`;
  $('#worker-countdown').hidden = true;
}
function renderDetail() {
  if (!selected) return;
  $('#welcome').hidden = true; $('#detail').hidden = false;
  $('#title').textContent = selected.title;
  const podName = selected.active?.podName || selected.lastExecution?.podName;
  $('#task-id').textContent = podName || '';
  $('#task-id').hidden = !podName;
  $('#task-draft').textContent = selected.draft ? 'Reference draft: ' + selected.draft.title : '';
  const controls = $('#context-controls'); controls.replaceChildren();
  for (const [key, control] of Object.entries(selected.controls || {})) {
    const button = node('button', `${control.label} · ${control.enabled ? 'on' : 'off'}`, 'context-knob' + (control.enabled ? ' enabled' : ''));
    button.type = 'button'; button.setAttribute('aria-pressed', String(control.enabled)); button.disabled = readOnly;
    button.title = `${control.description || control.label}\nChanges are passed to subsequent turns; the agent can reread them during a turn.`;
    button.onclick = async () => {
      button.disabled = true;
      try { selected = await api(`/tasks/${selected.id}/controls/${encodeURIComponent(key)}`, 'PATCH', {enabled: !control.enabled}); renderDetail(); }
      catch (error) { failure(error); renderDetail(); }
    };
    controls.append(button);
  }
  $('#share-banner').hidden = !readOnly;
  if (readOnly) {
    $('#share-banner').textContent = 'Read-only shared task';
    document.title = selected.title + ' · read-only';
  }
  const reference = selected.references?.find(r => r.source === 'github' && ['issue', 'pr', 'pull_request'].includes(r.kind));
  const repository = selected.repository || reference?.repository;
  const context = $('#task-repository'); context.replaceChildren();
  if (repository && /^[\w.-]+\/[\w.-]+$/.test(repository)) {
    const url = 'https://github.com/' + repository.split('/').map(encodeURIComponent).join('/');
    const link = node('a', repository); link.href = url; link.target = '_blank'; link.rel = 'noopener noreferrer'; link.title = url;
    context.append(link);
    if (reference?.id && /^\d+$/.test(String(reference.id))) {
      const issue = node('a', `#${reference.id}`);
      issue.href = url + (reference.kind === 'issue' ? '/issues/' : '/pull/') + reference.id;
      issue.target = '_blank'; issue.rel = 'noopener noreferrer';
      context.append(node('span', ' · '), issue);
    }
  }
  context.hidden = !repository;
  renderCountdown();
  $('#reason').textContent = selected.blocked ? confirmationLabel(selected) + ': ' + selected.blocked.summary
    : selected.error || '';
  const timeline = $('#timeline');
  const scrollTop = timeline.scrollTop;
  for (const details of timeline.querySelectorAll('details[data-expansion-key]')) expandedDetails.set(details.dataset.expansionKey, details.open);
  const stickToBottom = timeline.dataset.taskId !== selected.id || timeline.scrollHeight - timeline.clientHeight - scrollTop < 80;
  timeline.dataset.taskId = selected.id; timeline.replaceChildren();
  if (document.activeElement !== $('#task-title')) $('#task-title').value = selected.title;
  $('#delete-task').disabled = Boolean(selected.deleteRequested);
  $('#share-task').hidden = cfg.sharingMode === 'disabled';
  const events = selected.events.filter(e => !(e.kind === 'control' && (e.action === 'remove_prompt' || e.content === 'Removed queued prompt'))
    && !(e.kind === 'event' && selected.events.some(p => p.causedBy?.includes(e.id)))
    && (e.kind !== 'progress' || !selected.events.some(r =>
      r.kind === 'result' && r.executionId === e.executionId && r.content.slice(0, 8000) === e.content)));
  for (const entry of groupAgentMessages(events)) {
    if (entry.kind === 'control' || (entry.actor.type === 'controller' && entry.kind !== 'prompt')) {
      const line = node('div', '', 'timeline-activity');
      const icon = node('span', entry.kind === 'execution' ? '▶' : entry.kind === 'execution_stop' ? '⏹' : entry.kind === 'recovery' ? '↻' : '•', 'activity-icon');
      icon.setAttribute('aria-hidden', 'true');
      const eventPod = entry.podName || (entry.executionId ? `agent-worker-${selected.id.slice(0, 8)}-${entry.executionId.slice(0, 8)}` : undefined);
      const content = entry.kind === 'execution' && entry.content === 'Starting worker'
        ? `Starting ${eventPod || 'worker'}` : entry.kind === 'execution_stop' && entry.content === 'Stopping worker'
          ? `Stopping ${eventPod || 'worker'}` : entry.content;
      line.append(icon, node('span', content));
      line.title = `${entry.actor.name || entry.actor.type} · ${entry.kind} · ${new Date(entry.createdAt).toLocaleString()}`;
      line.tabIndex = 0; timeline.append(line); continue;
    }
    const n = node('article', '', `entry ${entry.actor.type}`);
    const author = entry.actor.type === 'controller' ? 'Controller' : entry.actor.type === 'agent' ? cfg.identity?.name || entry.actor.name || 'Coding agent' : entry.actor.name || entry.actor.subject || entry.actor.type;
    const meta = node('div', '', 'meta');
    meta.append(avatar(entry.actor), authorLink(entry.actor, author), node('span', '· ' + new Date(entry.createdAt).toLocaleString()));
    if (entry.kind === 'prompt') {
      const queued = selected.pending.includes(entry.id);
      const current = selected.active?.currentPrompts || (selected.active?.state !== 'Idle' ? selected.active?.prompts : []) || [];
      const running = !queued && current.includes(entry.id) && selected.desiredState === 'Active';
      const ready = selected.active?.podReady ?? selected.active?.state === 'Running';
      const status = entry.queueDisposition === 'stopped' ? 'Stopped' : entry.queueDisposition === 'stopping' ? 'Stopping' : entry.queueDisposition === 'removed' ? 'Removed' : queued ? (selected.desiredState === 'Paused' ? 'Queued · paused' : 'Queued')
        : current.includes(entry.id) ? (selected.desiredState !== 'Active' ? 'Stopping' : ready ? 'Running' : 'Starting worker') : 'Finished';
      const badge = node('span', '', 'prompt-status ' + (running ? 'running-badge' : queued ? 'queued-badge' : ''));
      if (running) { const spinner = node('i', '', 'prompt-spinner'); spinner.setAttribute('aria-hidden', 'true'); badge.append(spinner); }
      badge.append(node('span', status)); meta.append(badge);
      n.classList.toggle('prompt-running', running); n.classList.toggle('prompt-queued', queued);
      n.classList.toggle('prompt-removed', entry.queueDisposition === 'removed');
      if (queued && !readOnly) {
        const remove = node('button', '⌫', 'remove-prompt'); remove.type = 'button';
        remove.title = 'Remove this queued prompt'; remove.setAttribute('aria-label', remove.title);
        const taskId = selected.id;
        remove.onclick = async () => {
          remove.disabled = true;
          try { const updated = await api(`/tasks/${taskId}/prompts/${entry.id}`, 'DELETE', {});
            if (selected?.id === taskId) { selected = updated; renderDetail(); } await refresh(); }
          catch (error) { failure(error); await refresh().catch(failure); }
        };
        meta.append(remove);
      }
    } else if (entry.actor.type === 'controller' || entry.kind === 'event') meta.append(node('span', entry.kind || entry.source, 'prompt-status'));
    if (entry.kind === 'email') meta.append(node('span', 'Email · ' + entry.deliveryState, 'prompt-status'));
    n.append(meta, markdown(entry.content));
    if (entry.tools?.length) {
      const details = node('details','','tool-activity');
      details.dataset.expansionKey = `${selected.id}:${entry.id}:tools`;
      details.open = expandedDetails.get(details.dataset.expansionKey) || false;
      details.append(node('summary',`${entry.tools.length} tool call${entry.tools.length===1?'':'s'}`));
      for (const tool of entry.tools) {
        const row = node('div','','tool-call');
        row.append(node('div',`${tool.toolState==='running'?'◌':tool.toolState==='failed'?'✕':'✓'} ${tool.toolName || 'Tool'}${tool.toolDetail ? ' · ' + tool.toolDetail : ''} · ${tool.toolState || 'requested'}${Number.isInteger(tool.exitCode)?` · exit ${tool.exitCode}`:''}`,'tool-heading'));
        row.title = new Date(tool.createdAt).toLocaleString();
        if (tool.command) row.append(node('pre',tool.command));
        details.append(row);
      }
      n.append(details);
    }
    if (entry.metrics && Object.keys(entry.metrics).length) {
      const stats = node('small', responseMetrics(entry.metrics), 'response-metrics');
      stats.title = 'Turn elapsed time includes model requests, prompt processing, tool execution, and CLI overhead. Token counts are reported by the CLI; whole-turn throughput is not model decode speed.';
      n.append(stats);
    }
    if (entry.causedBy?.length) {
      const details = node('details', '', 'prompt-context'); details.append(node('summary', 'Source event'));
      details.dataset.expansionKey = `${selected.id}:${entry.id}:source`;
      details.open = expandedDetails.get(details.dataset.expansionKey) || false;
      for (const cause of entry.causedBy) {
        const source = selected.events.find(e => e.id === cause);
        details.append(node('pre', source ? `${source.actor.name || source.actor.type}: ${source.content}` : cause));
      }
      n.append(details);
    }
    timeline.append(n);
  }
  timeline.scrollTop = stickToBottom ? timeline.scrollHeight : scrollTop;
  const terminal = ['Completed', 'Cancelled', 'Failed'].includes(selected.phase);
  for (const b of $('#controls').querySelectorAll('[data-state]')) {
    b.disabled = terminal || (b.dataset.state === 'Completed' && Boolean(selected.active));
    if (b.dataset.state === 'Paused') b.hidden = terminal || selected.desiredState !== 'Active';
    if (b.dataset.state === 'Active') { b.hidden = terminal || selected.desiredState !== 'Paused'; b.disabled = Boolean(selected.active); }
  }
  const busy = Boolean(selected.active && selected.active.state !== 'Idle' && selected.desiredState === 'Active');
  $('#stop-prompt').hidden = !busy;
  $('#stop-prompt').disabled = !selected.active?.supportsPromptStop || Boolean(selected.active?.interrupt);
  $('#stop-prompt').title = !selected.active?.supportsPromptStop ? selected.active?.turnStartedAt
    ? 'Restart this worker once to enable prompt-only stop' : 'Waiting for the worker to start'
    : selected.active?.interrupt ? 'Stopping current prompt' : 'Stop current prompt; keep Pod';
  const queued = selected.pending.length;
  $('#processing-state').textContent = terminal ? 'Task closed' : selected.desiredState === 'Paused'
    ? `Paused${queued ? ` · ${queued} prompt(s) queued` : ''}` : busy
      ? `${selected.active.podReady ? `${cfg.identity?.name || 'Coding agent'} is processing your prompts…` : 'Worker is starting…'}${queued ? ` · ${queued} prompt(s) queued for the next turn` : ''}`
      : queued ? `${queued} prompt(s) queued${selected.active ? ' · waiting for agent to pick up' : ' · waiting for worker'}` : selected.blocked ? confirmationLabel(selected) : 'Ready for your next prompt';
  $('#followup button').disabled = terminal || sending;
  $('#followup button').textContent = sending ? 'Sending…' : busy || selected.desiredState === 'Paused' ? 'Queue →' : 'Send →';
  if (!modelDirty) {
    $('#task-backend').value = selected.model?.backend || 'claude';
    populateModels('#task-model', $('#task-backend').value, selected.model?.id);
  }
  const editable = !terminal && selected.desiredState === 'Paused' && !selected.active;
  for (const element of $('#change-model').querySelectorAll('select,button')) element.disabled = !editable;
}
async function select(id) {
  if (id !== selected?.id) modelDirty = false;
  selected = await api('/tasks/' + id);
  history.replaceState({}, '', '/?task=' + encodeURIComponent(id));
  renderList(); renderDetail(); renderDeviceNavbar();
}
async function refresh() {
  const listing = await api('/tasks'); tasks = listing.tasks;
  renderConnectivity(listing.connections); renderList();
  if (selected) await select(selected.id);
}
$('#login').onclick = async () => {
  try {
    if (user && !user.expired) { $('#connectivity-navbar').hidden = true; await auth.removeUser(); location.replace('/'); return; }
    await auth.signinRedirect({state: !$('#task-composer').hidden ? {composer: composerValue()}
      : {task: new URLSearchParams(location.search).get('task')}});
  } catch (error) { failure(error); }
};
async function showInstructions() {
  const button = $('#refresh-instructions'); button.disabled = true;
  try {
    const value = await api('/instructions');
    $('#instructions-content').textContent = value.content;
    $('#instructions-source').textContent = `${value.source} · ${value.revision.slice(0, 12)} · Read ${new Date(value.readAt).toLocaleTimeString()}`;
    if (!$('#instructions-dialog').open) $('#instructions-dialog').showModal();
  } catch (error) { failure(error); }
  finally { button.disabled = false; }
}
$('#instructions').onclick = showInstructions;
$('#refresh-instructions').onclick = showInstructions;
$('#close-instructions').onclick = () => $('#instructions-dialog').close();
$('#connections').onclick = async () => {
  try {
    const urls = await api('/connections');
    $('#github-url').textContent = urls.github;
    $('#mail-status').textContent = `IMAP: ${urls.mailbox.imap.state} · SMTP: ${urls.mailbox.smtp.state}${urls.mailbox.smtp.from ? ' · ' + urls.mailbox.smtp.from : ''}`;
    $('#alert-example').textContent = `receivers:\n  - name: codemowers-agent\n    webhook_configs:\n      - url: ${urls.alertmanager}\n        send_resolved: true\n        http_config:\n          authorization:\n            type: Bearer\n            credentials: <receiver bearer token>\n\n# Add a matching route to your existing route tree:\n# receiver: codemowers-agent`;
    $('#wakeup-example').textContent = `POST ${urls.wakeup}\nAuthorization: Bearer <webhook bearer token>\nContent-Type: application/json\n\n{"deliveryId":"unique-event-id","taskId":"<Task ID>","context":"Continue implementation"}\n\n# Alternatively replace taskId with issueURL.\n# issueURL: https://github.com/owner/repo/issues/42`;
    $('#setup').showModal();
  } catch (error) { failure(error); }
};
$('#close-setup').onclick = () => {
  $('#setup').close();
  for (const element of $('#setup').querySelectorAll('[id$="-credential"]')) element.textContent = '';
};
for (const button of $('#setup').querySelectorAll('[data-credential]')) button.onclick = async () => {
  try { const value = await api('/connections/' + button.dataset.credential + '/credential', 'POST', {});
    $('#' + button.dataset.credential + '-credential').textContent = value.credential; }
  catch (error) { failure(error); }
};
function openComposer(step = 'choose') {
  // DOM click handlers pass MouseEvent; direct callers pass the wizard step.
  if (typeof step !== 'string') step = 'choose';
  composerStep = step === 'choose' ? 'choose' : 'edit';
  $('#clear-draft').click(); chooseRepository(null); $('#objective').value = '';
  $('#task-composer-title').textContent = 'New task';
  $('#start-task').disabled = false;
  $('#start-task').textContent = user && !user.expired ? 'Start task →' : 'Sign in to start task →';
  selected = null; readOnly = false; publicDirectory = false;
  document.body.classList.remove('read-only');
  $('#detail').hidden = true; $('#welcome').hidden = false;
  $('#new').hidden = false;
  $('#drafts-section').hidden = step !== 'choose';
  $('#task-composer').hidden = step === 'choose';
  $('#welcome-title').textContent = step === 'choose' ? 'Choose a starting point' : 'New task';
  $('#welcome-description').textContent = step === 'choose' ? 'Start from scratch or choose a specification to adapt.' : 'Describe the work and review your specification before starting.';
  history.replaceState({}, '', '/');
  $('#welcome').scrollTop = 0;
  void refreshDrafts().catch(failure);
  if (step !== 'choose') $('#objective').focus({preventScroll: true});
  if (user && !user.expired && step !== 'choose') void refreshResources();
}
$('#new').onclick = openComposer;
$('#browse-drafts').onclick = () => openComposer('choose');
$('#close-task-composer').onclick = () => {
  composerStep = 'choose';
  $('#task-composer').hidden = true; $('#drafts-section').hidden = false;
  $('#welcome-title').textContent = 'Choose a starting point';
  $('#welcome-description').textContent = 'Start from scratch or choose a specification to adapt.';
  $('#welcome').scrollTop = 0;
};
async function prepareRefinement(taskId) {
  const source = selected?.id === taskId ? selected : await api('/tasks/' + encodeURIComponent(taskId));
  openComposer('scratch');
  selectedRefinement = source.id;
  chooseRepository(refinementRepository);
  $('#draft-selection').textContent = 'Reference transcript: ' + source.title;
  $('#clear-draft').hidden = false;
  $('#objective').value = refinementPrompt;
  $('#reference-content').value = transcriptText(source);
  $('#reference-editor').hidden = false;
  $('#task-composer-title').textContent = 'Refine: ' + source.title;
}
$('#refine-task').onclick = async () => {
  const taskId = selected.id;
  try {
    await prepareRefinement(taskId);
  } catch (error) { failure(error); }
};
$('#rename').onsubmit = async event => {
  event.preventDefault();
  try { selected = await api(`/tasks/${selected.id}`, 'PATCH', {title: $('#task-title').value, revision: selected.revision}); renderDetail(); await refresh(); }
  catch (error) { failure(error); }
};
$('#delete-task').onclick = async () => {
  try { await api(`/tasks/${selected.id}`, 'DELETE', {}); selected = null; history.replaceState({}, '', '/'); $('#detail').hidden = true; $('#welcome').hidden = false; await refresh(); }
  catch (error) { failure(error); }
};
$('#share-task').onclick = async () => {
  const button = $('#share-task'); button.disabled = true;
  try {
    const taskId = selected.id;
    const value = await api(`/tasks/${taskId}/share`, 'POST', {});
    shareTaskId = taskId; $('#share-url').value = value.url;
    $('#share-description').textContent = cfg.sharingMode === 'public'
      ? 'This task is listed publicly. Anyone can view its conversation and status without signing in. They cannot send prompts or control the task.'
      : 'Anyone with this signed link can view the conversation and status without signing in. They cannot send prompts or control the task.';
    $('#share-expiry').textContent = 'Valid until revoked or the task is deleted.';
    $('#copy-share').textContent = 'Copy link'; $('#share-dialog').showModal();
  } catch (error) { failure(error); }
  finally { button.disabled = false; }
};
$('#stop-prompt').onclick = async () => {
  const button = $('#stop-prompt'); button.disabled = true;
  try { selected = await api(`/tasks/${selected.id}/interrupt`, 'POST', {executionId: selected.active.id,
    promptIds: selected.active.currentPrompts}); renderDetail(); }
  catch (error) { failure(error); renderDetail(); }
};
$('#close-share').onclick = () => $('#share-dialog').close();
$('#copy-share').onclick = async () => {
  try { await navigator.clipboard.writeText($('#share-url').value); $('#copy-share').textContent = 'Copied'; }
  catch { $('#share-url').focus(); $('#share-url').select(); $('#copy-share').textContent = 'Select and copy'; }
};
$('#revoke-share').onclick = async () => {
  try { await api(`/tasks/${shareTaskId}/share`, 'DELETE', {}); $('#share-dialog').close(); $('#share-url').value = ''; }
  catch (error) { failure(error); }
};
$('#create').onsubmit = async event => {
  event.preventDefault(); const button = $('#start-task'); button.disabled = true;
  const composer = composerValue();
  try {
    if (!user || user.expired) { await auth.signinRedirect({state: {composer}}); return; }
    const task = await api('/tasks', 'POST', composer);
    $('#task-composer').hidden = true; $('#objective').value = ''; $('#clear-draft').click(); chooseRepository(null);
    await refresh(); await select(task.id);
  } catch (error) { failure(error); } finally { button.disabled = false; }
};
function composerValue() {
  return {prompt: $('#objective').value, backend: $('#backend').value, modelId: $('#model').value || undefined,
    repository: selectedRepository || undefined, draftId: selectedDraft?.id, draftTitle: selectedDraft?.title,
    refineTaskId: selectedRefinement || undefined, referenceContent: $('#reference-editor').hidden ? undefined : $('#reference-content').value};
}
$('#change-model').onsubmit = async event => {
  event.preventDefault();
  try { selected = await api(`/tasks/${selected.id}/model`, 'POST', {backend: $('#task-backend').value,
    modelId: $('#task-model').value, revision: selected.revision}); modelDirty = false; renderDetail(); }
  catch (error) { failure(error); }
};
$('#followup').onsubmit = async event => {
  event.preventDefault();
  if (sending) return;
  const taskId = selected.id, content = $('#message').value;
  sending = true; renderDetail();
  try { const task = await api(`/tasks/${taskId}/prompts`, 'POST', {prompt: content});
    if (selected?.id === taskId) { selected = task; if ($('#message').value === content) $('#message').value = ''; } }
  catch (error) { failure(error); }
  finally { sending = false; renderDetail(); }
};
let knownDevices = [];
async function refreshDevices() {
  if (!user || user.expired) return;
  const data = await api('/devices');
  knownDevices = data.devices;
  renderDeviceNavbar();
}
function renderDeviceNavbar() {
  const nav = $('#devices-navbar'); nav.replaceChildren();
  for (const device of knownDevices.filter(d => d.connected)) {
    const included = selected?.devices?.includes(device.id);
    const btn = node('button', '', 'device-badge' + (included ? ' device-included' : ''));
    btn.type = 'button';
    btn.title = (device.hostname || device.label || device.id.slice(0, 8)) + (included ? ' · included in task (click to remove)' : ' · click to include in task');
    btn.setAttribute('aria-pressed', String(Boolean(included)));
    const dot = node('i', '', 'state-dot ' + (included ? 'green' : 'gray')); dot.setAttribute('aria-hidden', 'true');
    btn.append(dot, node('span', device.hostname || device.label || device.id.slice(0, 8)));
    btn.onclick = () => toggleDevice(device.id, !included);
    nav.append(btn);
  }
}
async function toggleDevice(deviceId, include) {
  if (!selected) return;
  try { selected = await api(`/tasks/${selected.id}/devices/${encodeURIComponent(deviceId)}`, 'PATCH', {included: include}); renderDeviceNavbar(); renderDetail(); }
  catch (error) { failure(error); }
}
function renderDeviceList() {
  const list = $('#device-list'); list.replaceChildren();
  if (!knownDevices.length) { list.append(node('p', 'No devices registered yet.', 'empty-resource')); return; }
  for (const device of knownDevices) {
    const row = node('div', '', 'resource-row');
    const label = node('strong', device.hostname || device.label || device.id.slice(0, 8));
    const status = node('small', device.connected ? `Connected since ${new Date(device.connectedAt).toLocaleString()}` : 'Disconnected');
    const url = node('code', device.url); url.style.display = 'block'; url.style.fontSize = '11px'; url.style.overflowWrap = 'anywhere';
    const copyBtn = node('button', 'Copy URL', 'copy-url-btn'); copyBtn.type = 'button';
    copyBtn.onclick = async () => { try { await navigator.clipboard.writeText(device.url); copyBtn.textContent = 'Copied'; setTimeout(() => { copyBtn.textContent = 'Copy URL'; }, 2000); } catch { copyBtn.textContent = 'Copy failed'; } };
    const revokeBtn = node('button', 'Revoke', 'revoke-device-btn'); revokeBtn.type = 'button';
    revokeBtn.onclick = async () => { revokeBtn.disabled = true; try { await api('/devices/' + encodeURIComponent(device.id), 'DELETE', {}); await refreshDevices(); renderDeviceList(); } catch (error) { failure(error); revokeBtn.disabled = false; } };
    row.append(label, status, url, copyBtn, revokeBtn);
    list.append(row);
  }
}
$('#manage-devices').onclick = async () => {
  try { await refreshDevices(); renderDeviceList(); $('#devices-dialog').showModal(); }
  catch (error) { failure(error); }
};
$('#close-devices').onclick = () => $('#devices-dialog').close();
$('#add-device').onclick = async () => {
  const btn = $('#add-device'); btn.disabled = true;
  try { await api('/devices', 'POST', {}); await refreshDevices(); renderDeviceList(); }
  catch (error) { failure(error); }
  finally { btn.disabled = false; }
};

for (const textarea of [$('#objective'), $('#message')]) {
  textarea.title = 'Enter to send; Shift+Enter for a new line';
  textarea.addEventListener('keydown', event => {
    if (event.key !== 'Enter' || event.shiftKey || event.isComposing || event.keyCode === 229) return;
    event.preventDefault();
    const button = textarea.form.querySelector('button[type="submit"]');
    if (!button.disabled && textarea.value.trim()) textarea.form.requestSubmit(button);
  });
}
for (const b of $('#controls').querySelectorAll('[data-state]')) b.onclick = async () => {
  b.disabled = true;
  try { selected = await api(`/tasks/${selected.id}/state`, 'POST', {state: b.dataset.state, revision: selected.revision}); renderDetail(); await refresh(); }
  catch (error) { failure(error); await refresh().catch(failure); }
  finally { renderDetail(); }
};
setInterval(renderCountdown, 1000);
async function refreshShared() {
  if (!readOnly) return;
  const response = sharedTaskId ? await fetch('/shared/tasks/' + encodeURIComponent(sharedTaskId))
    : await fetch('/shared/task', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({token: shareToken})});
  if (!response.ok) {
    if (response.status === 404) { selected = null; $('#detail').hidden = true; $('#welcome').hidden = true; }
    throw new Error(response.status === 404 ? 'This share link was revoked or the task was deleted.' : 'Shared task is temporarily unavailable.');
  }
  const value = await response.json(); selected = value.task;
  renderDetail(); $('#error').hidden = true;
}
async function refreshSharedDirectory() {
  publicDirectory = true;
  $('#task-list-title').textContent = 'Shared transcripts'; $('#new').hidden = true;
  $('#welcome-title').textContent = 'Shared transcripts';
  $('#welcome-description').textContent = 'Choose a public transcript from the list. Sign in to create and manage tasks.';
  tasks = await fetch('/shared/tasks').then(responseJson); renderList();
  if (!tasks.length) $('#tasks').append(node('p', 'No shared transcripts yet.', 'empty-resource'));
}
try {
  if (readOnly) {
    $('#welcome').hidden = true;
    await refreshShared();
    setInterval(() => refreshShared().catch(failure), 5000);
  } else {
  await refreshDrafts();
  let restoredComposer;
  if (location.pathname === '/auth/callback') {
    user = await auth.signinRedirectCallback(); history.replaceState({}, '', user.state?.refine ? '/?refine=' + encodeURIComponent(user.state.refine) : user.state?.draft ? '/?draft=' + encodeURIComponent(user.state.draft) : user.state?.task ? '/?task=' + encodeURIComponent(user.state.task) : '/');
    restoredComposer = user.state?.composer;
  } else user = await auth.getUser();
  if (user && !user.expired) {
    const actor = await api('/me'); currentActor = actor;
    $('#connectivity-navbar').hidden = false;
    void loadConnectivity();
    void refreshDevices();
    $('#login').textContent = '⏻'; $('#login').title = 'Sign out · ' + actor.name;
    $('#login').setAttribute('aria-label', 'Sign out');
    $('#new').disabled = false; $('#start-task').disabled = false; $('#signin-note').hidden = true;
    $('#connections').disabled = false;
    $('#instructions').disabled = false;
    $('#manage-devices').disabled = false;
    await refreshModels(); await refresh();
    void refreshResources();
    const taskId = new URLSearchParams(location.search).get('task');
    if (taskId) await select(taskId);
    const draftId = new URLSearchParams(location.search).get('draft');
    if (draftId) await useDraft(draftId);
    const refineId = new URLSearchParams(location.search).get('refine');
    if (refineId) await prepareRefinement(refineId);
    if (restoredComposer) {
      openComposer('scratch');
      selectedDraft = restoredComposer.draftId ? {id: restoredComposer.draftId, title: restoredComposer.draftTitle} : null;
      selectedRefinement = restoredComposer.refineTaskId || null;
      chooseRepository(restoredComposer.repository || null);
      $('#objective').value = restoredComposer.prompt;
      $('#backend').value = restoredComposer.backend;
      populateModels('#model', restoredComposer.backend, restoredComposer.modelId);
      if (restoredComposer.referenceContent !== undefined) {
        $('#reference-content').value = restoredComposer.referenceContent; $('#reference-editor').hidden = false;
        $('#draft-selection').textContent = selectedDraft ? 'Reference draft: ' + selectedDraft.title : 'Reference transcript';
        $('#clear-draft').hidden = false;
      }
    }
    setInterval(() => { refresh().catch(failure); refreshDevices().catch(failure); }, 5000);
  } else if (cfg.sharingMode === 'public') {
    await refreshSharedDirectory();
    setInterval(() => { if (publicDirectory) refreshSharedDirectory().catch(failure); }, 10000);
  }
  }
} catch (error) { failure(error); }
