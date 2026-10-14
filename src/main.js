import {createClient} from 'redis';
import express from 'express';
import {readFile} from 'node:fs/promises';
import {createServer} from 'node:https';
import {randomUUID, createHmac} from 'node:crypto';
import {Store} from './store.js';
import {Kube} from './kube.js';
import {reconcile} from './controller.js';
import {authentication} from './auth.js';
import {ModelCatalog, changeModel} from './models.js';
import {acceptsGithubEvent, githubTask, githubWakeup, closeGithubTask, reopenGithubTask, reconcileGithubIssues, pullRequestReference, reconcileGithubPullRequests} from './github.js';
import {Mailbox} from './mailbox.js';
import {agentIdentity, agentActor, displayIdentity} from './identity.js';
import {installFaultHandler, createRepairTask, faultDetails, repairContext} from './faults.js';
import {githubNotificationReference, resolveGithubMailIssues, routeGithubIssueMail} from './github-mail.js';
import {webhookLog} from './webhook-log.js';
import {SetupCatalog} from './setup.js';
import {ConnectivityCatalog} from './connectivity.js';
import {agentMcpInitialize, taskCommunication} from './agent-mcp.js';
import {BuildCatalog} from './builds.js';
import {DeviceCatalog} from './devices.js';
import {WebSocketServer} from 'ws';
import {SkaffoldSchema} from './skaffold-schema.js';
import {SharedLinks} from './sharing.js';
import {DraftCatalog} from './drafts.js';
import {transcriptDraft} from './refinement.js';
import {refinementRepository, refinementPrompt} from './refinement-defaults.js';
import {Problem, text, id, newTask, prompt, activity, controllerActor, publicTask, changeState, digest, tokenMatches,
  workerEvent, claimNextTurn, renameTask, deleteTask, removeQueuedPrompt, stateRevision, blockExecution, interruptPrompt,
  normalizeControls, controlsFromMarkdown, setControl} from './domain.js';

const faults = installFaultHandler();
if (process.env.AGENT_MODE === 'worker') {
  faults.setReporter(async report => {
    const response = await fetch(`${process.env.AGENT_SERVICE_URL}/executions/${process.env.AGENT_TASK_ID}/fault`, {
      method: 'POST', headers: {'Content-Type': 'application/json', Authorization: `Bearer ${process.env.AGENT_REPORT_TOKEN}`},
      body: JSON.stringify(report), signal: AbortSignal.timeout(8000), redirect: 'error'});
    if (!response.ok) throw new Error('Exception report rejected');
    return response.json();
  });
  const {worker} = await import('./worker.js');
  await worker((error, kind) => faults.report(error, kind));
} else {
  const required = name => {
    if (!process.env[name]) throw new Error(name + ' is required');
    return process.env[name];
  };
  const config = {
    namespace: required('AGENT_NAMESPACE'),
    release: required('AGENT_RELEASE'), image: required('AGENT_WORKER_IMAGE'),
    instance: process.env.HOSTNAME || randomUUID(),
    identity: agentIdentity(),
    issuer: required('OIDC_IDP_URI'), origin: required('OIDC_CLIENT_ORIGIN'),
    audience: required('OIDC_CLIENT_ORIGIN') + '/api',
    clientId: required('OIDC_CLIENT_ID'), redirectURI: required('OIDC_REDIRECT_URIS').split(/[ ,\n]+/)[0],
    allowedGroups: (process.env.OIDC_ALLOWED_GROUPS || '').split(',').filter(Boolean),
    allowedSubjects: (process.env.OIDC_ALLOWED_SUBJECTS || '').split(',').filter(Boolean),
    storageClass: required('AGENT_STORAGE_CLASS'), workspaceSize: process.env.AGENT_WORKSPACE_SIZE || '10Gi',
    anthropicSecret: required('AGENT_ANTHROPIC_SECRET'), githubSecret: required('AGENT_GITHUB_SECRET'),
    internalURL: required('OIDC_CLIENT_URI').replace(/\/$/, ''), model: process.env.AGENT_MODEL || 'claude-sonnet-4-6',
    budget: Number(process.env.AGENT_BUDGET_USD || '5'),
    kubeContext: process.env.AGENT_KUBE_CONTEXT,
    toolImages: JSON.parse(process.env.AGENT_TOOL_IMAGES || '[]'),
    openaiSecret: process.env.AGENT_OPENAI_SECRET, codexModel: process.env.AGENT_CODEX_MODEL || '',
    sharingMode: process.env.AGENT_SHARING_MODE || 'disabled',
  };
  config.namespaceIdentity = {name: config.namespace, source: 'Pod metadata.namespace fieldRef'};
  const redis = createClient({url: required('REDIS_URL'), password: process.env.REDIS_PASSWORD});
  redis.on('error', error => console.error(JSON.stringify({level: 'error', component: 'redis', message: error.message})));
  await redis.connect();
  const store = new Store(redis, config.namespace, config.release), kube = new Kube(config);
  const devices = new DeviceCatalog(kube, config);
  faults.setReporter(report => createRepairTask(store, config.namespace, report));
  const models = new ModelCatalog(config);
  config.probeAgent = async () => { await redis.ping(); return agentMcpInitialize(); };
  const setup = new SetupCatalog(config);
  const builds = new BuildCatalog(kube);
  const skaffold = new SkaffoldSchema();
  const drafts = new DraftCatalog();
  const mailbox = new Mailbox(store, undefined, {onException: (error, kind) => faults.report(error, kind),
    onGithubNotification: handleGithubMail});
  const connectivity = new ConnectivityCatalog(setup, mailbox, builds);
  void connectivity.start().then(({connections}) => console.log(JSON.stringify({event: 'connectivity_checked',
    checks: connections.map(({name, status, detail, checkedAt, probeReference}) => ({name, status, detail, checkedAt, ...(probeReference ? {probeReference} : {})}))})));
  config.syncMailbox = () => mailbox.tick();
  config.cleanupTask = task => mailbox.removeTask(task.id);
  const sharingKey = config.sharingMode === 'disabled' ? undefined : process.env.AGENT_SHARING_SIGNING_KEY
    || createHmac('sha256', required('WEBHOOK_TOKEN')).update('codemowers/agent-sharing:' + config.namespace + ':' + config.release).digest('hex');
  const shares = new SharedLinks(store, {mode: config.sharingMode, key: sharingKey});
  const authenticate = await authentication(config);
  const app = express();
  app.disable('x-powered-by');
  app.use('/hooks', webhookLog());
  app.use(express.json({limit: '256kb', verify: (req, res, bytes) => { req.rawBody = bytes; }}));
  app.use((req, res, next) => {
    res.setHeader('X-Content-Type-Options', 'nosniff');
    res.setHeader('Referrer-Policy', 'no-referrer');
    res.setHeader('Cache-Control', 'no-store');
    next();
  });
  app.get('/health', async (req, res) => { await redis.ping(); res.json({ok: true}); });
  // Signed links authorize only read-only sharing routes, never /api mutations.
  app.post('/shared/task', async (req, res) => res.json(await shares.read(req.body.token)));
  app.get('/shared/tasks', async (req, res) => res.json(await shares.list()));
  app.get('/shared/tasks/:id', async (req, res) => res.json(await shares.publicRead(id(req.params.id))));
  app.get('/drafts', async (req, res) => res.json(await drafts.list()));
  app.get('/drafts/:id', async (req, res) => res.json(await drafts.get(req.params.id)));
  app.get('/config', (req, res) => res.json({issuer: config.issuer, clientId: config.clientId,
    audience: config.audience, redirectURI: config.redirectURI, namespace: config.namespace, sharingMode: config.sharingMode,
    identity: displayIdentity(), scopes: 'openid profile email groups namespaces agent:read agent:write'}));
  app.use('/api', async (req, res, next) => {
    req.actor = await authenticate(req, req.method === 'GET' ? 'agent:read' : 'agent:write');
    next();
  });
  const requestId = req => text(req.headers['idempotency-key'], 200);
  app.get('/api/me', (req, res) => res.json(req.actor));
  app.get('/api/connectivity', async (req, res) => res.json(await connectivity.snapshot()));
  app.get('/api/models', async (req, res) => res.json(await models.list()));
  app.get('/api/setup', async (req, res) => {
    const [catalog, infrastructure] = await Promise.all([setup.list(req.query.refresh === 'true'), builds.list()]);
    res.json({...catalog, infrastructure});
  });
  app.get('/api/instructions', async (req, res) => {
    const content = await readFile('/config/instructions.md', 'utf8');
    res.json({content, revision: digest(content), source: config.release + '-config / instructions.md',
      readAt: new Date().toISOString()});
  });
  app.get('/api/mcps', async (req, res) => res.json({mcps: await setup.mcps()}));
  app.get('/api/connections', async (req, res) => res.json({base: config.internalURL,
    github: config.internalURL + '/hooks/github', wakeup: config.internalURL + '/hooks/wakeup',
    alertmanager: config.internalURL + '/hooks/alertmanager', mailbox: await mailbox.status()}));
  app.get('/api/mailbox', async (req, res) => res.json(await mailbox.status()));
  app.get('/api/devices', async (req, res) => res.json({devices: await devices.list()}));
  app.post('/api/devices', async (req, res) => {
    const label = req.body.label ? text(req.body.label, 100) : null;
    res.status(201).json(await devices.create(label));
  });
  app.delete('/api/devices/:key', async (req, res) => {
    await devices.revoke(req.params.key);
    res.json({ok: true});
  });
  app.patch('/api/tasks/:id/devices/:key', async (req, res) => {
    const key = req.params.key;
    res.json(publicTask(await store.mutate(id(req.params.id), requestId(req), task => {
      if (!task.devices) task.devices = [];
      if (req.body.included) { if (!task.devices.includes(key)) task.devices.push(key); }
      else task.devices = task.devices.filter(k => k !== key);
    })));
  });
  app.post('/api/connections/:type/credential', async (req, res) => {
    const names = {github: 'GITHUB_WEBHOOK_SECRET', wakeup: 'WEBHOOK_TOKEN', alertmanager: 'ALERTMANAGER_TOKEN'};
    const name = names[req.params.type];
    if (!name) throw new Problem(404, 'Unknown connection');
    res.json({credential: process.env[name]});
  });
  app.get('/api/tasks', async (req, res) => res.json({tasks: (await store.list()).map(task => publicTask(task, false))
    .sort((a, b) => b.updatedAt.localeCompare(a.updatedAt)), ...await connectivity.snapshot()}));
  app.post('/api/tasks', async (req, res) => {
    if (req.body.refineTaskId && req.body.draftId) throw new Problem(400, 'Choose a transcript or a draft');
    const source = req.body.refineTaskId ? await store.get(id(req.body.refineTaskId)) : null;
    if (source?.deleteRequested) throw new Problem(404, 'Source task was deleted');
    let draft = source ? transcriptDraft(source)
      : req.body.draftId ? await drafts.get(req.body.draftId) : null;
    if (req.body.referenceContent !== undefined) {
      if (!draft) throw new Problem(400, 'An edited reference requires a draft or source task');
      const content = text(req.body.referenceContent, 128000);
      if (Buffer.byteLength(content) > 128000) throw new Problem(400, 'Reference text exceeds 128 KB');
      draft = {...draft, content, digest: digest(content), controls: source ? {} : controlsFromMarkdown(content)};
    }
    const task = newTask(config.namespace, req.actor, text(req.body.prompt || (source ? refinementPrompt
      : draft ? 'Implement the attached draft: ' + draft.title : '')));
    if (draft) { task.draft = {id: draft.id, title: draft.title, digest: draft.digest, content: draft.content}; task.controls = {...draft.controls, ...task.controls}; }
    if (req.body.controls) task.controls = {...task.controls, ...normalizeControls(req.body.controls)};
    task.controls = normalizeControls(task.controls);
    task.model = await models.select(req.body.modelId, req.body.backend);
    if (req.body.refineTaskId) {
      if (req.body.repository && req.body.repository !== refinementRepository) throw new Problem(400, 'Refinement targets codemowers/agent');
      task.repository = await setup.repository(refinementRepository);
      task.refinedFrom = id(req.body.refineTaskId);
    } else if (req.body.repository) task.repository = await setup.repository(req.body.repository);
    res.status(201).json(publicTask(await store.create(task, req.actor.issuer + ':' + req.actor.subject + ':' + requestId(req))));
  });
  app.get('/api/tasks/:id', async (req, res) => res.json(publicTask(await store.get(id(req.params.id)))));
  app.post('/api/tasks/:id/share', async (req, res) => {
    const link = await shares.create(id(req.params.id));
    res.status(201).json({url: new URL('/#share=' + link.token, config.origin).href});
  });
  app.delete('/api/tasks/:id/share', async (req, res) => {
    await store.mutate(id(req.params.id), requestId(req), task => {
      task.shareEpoch = (task.shareEpoch || 0) + 1; delete task.shared;
    });
    res.json({ok: true});
  });
  app.patch('/api/tasks/:id', async (req, res) => {
    if (!Number.isInteger(req.body.revision)) throw new Problem(400, 'Expected Task revision');
    res.json(publicTask(await store.mutate(id(req.params.id), requestId(req),
      task => renameTask(task, req.body.title, req.actor), req.body.revision)));
  });
  app.delete('/api/tasks/:id', async (req, res) => {
    const taskId = id(req.params.id);
    try { await store.mutate(taskId, requestId(req), task => deleteTask(task, req.actor)); }
    catch (error) { if (error.status !== 404) throw error; }
    res.status(202).json({ok: true});
  });
  app.post('/api/tasks/:id/model', async (req, res) => {
    const model = await models.select(req.body.modelId, req.body.backend);
    if (!Number.isInteger(req.body.revision)) throw new Problem(400, 'Expected Task revision');
    res.json(publicTask(await store.mutate(id(req.params.id), requestId(req),
      task => changeModel(task, model, req.actor), req.body.revision)));
  });
  app.post('/api/tasks/:id/prompts', async (req, res) => res.json(publicTask(await store.mutate(id(req.params.id), requestId(req),
    task => prompt(task, req.actor, text(req.body.prompt), 'webui')))));
  app.post('/api/tasks/:id/interrupt', async (req, res) => res.json(publicTask(await store.mutate(id(req.params.id), requestId(req),
    task => interruptPrompt(task, req.body.executionId, req.body.promptIds, req.actor)))));
  app.patch('/api/tasks/:id/controls/:key', async (req, res) => res.json(publicTask(await store.mutate(id(req.params.id), requestId(req),
    task => setControl(task, req.params.key, req.body.enabled, req.actor)))));
  app.delete('/api/tasks/:id/prompts/:promptId', async (req, res) => res.json(publicTask(await store.mutate(id(req.params.id), requestId(req),
    task => removeQueuedPrompt(task, id(req.params.promptId), req.actor)))));
  app.post('/api/tasks/:id/state', async (req, res) => {
    const expectedRevision = stateRevision(req.body.state, req.body.revision);
    res.json(publicTask(await store.mutate(id(req.params.id), requestId(req),
      task => changeState(task, req.body.state, req.actor), expectedRevision)));
  });

  async function execution(req) {
    const task = await store.get(id(req.params.id));
    if (!task.active || !tokenMatches(req.headers.authorization?.replace(/^Bearer /, ''), task.active.token)) {
      throw new Problem(401, 'Invalid execution binding');
    }
    return task;
  }
  app.get('/executions/:id', async (req, res) => {
    const task = await execution(req);
    res.json({id: task.id, objective: task.objective, desiredState: task.desiredState,
      session: task.session, repository: task.repository,
      prompts: task.events.filter(e => task.active.prompts.includes(e.id))});
  });
  app.post('/executions/:id/fault', async (req, res) => {
    const task = await execution(req);
    if (!['uncaughtException', 'unhandledRejection', 'worker_turn'].includes(req.body.kind)) throw new Problem(400, 'Invalid process exception kind');
    const repair = await createRepairTask(store, config.namespace, {component: 'worker', kind: req.body.kind,
      processId: task.active.id, sourceTaskId: task.id, executionId: task.active.id,
      details: faultDetails(text(req.body.details, 6000))});
    res.status(201).json({id: repair.id});
  });
  app.get('/executions/:id/infrastructure', async (req, res) => {
    await execution(req);
    res.json(await builds.list());
  });
  app.get('/executions/:id/control', async (req, res) => {
    const task = await execution(req);
    res.json({interrupt: task.active.interrupt || null, idleTimeoutSeconds: task.active.idleTimeoutSeconds || 0});
  });
  app.get('/executions/:id/context', async (req, res) => {
    const task = await execution(req);
    res.json({infrastructure: await builds.list(), controls: task.controls || {},
      identity: config.identity, email: mailbox.context(), communication: taskCommunication(task),
      repair: repairContext(task),
      draft: task.draft ? {id: task.draft.id, title: task.draft.title, path: '/execution/draft.md'} : null});
  });
  app.post('/executions/:id/next', async (req, res) => {
    let task = await execution(req);
    if (!Number.isInteger(req.body.afterSequence)) throw new Problem(400, 'Expected previous turn sequence');
    if (task.active.lastTurnSequence !== req.body.afterSequence) {
      res.json({desiredState: task.desiredState, prompts: []}); return;
    }
    if (task.active.state === 'Idle' && task.pending.length && task.desiredState === 'Active') {
      task = await store.mutate(task.id, 'next:' + task.active.id + ':' + text(req.body.id, 200),
        current => claimNextTurn(current, task.active.id));
      res.json({desiredState: task.desiredState, session: task.session,
        prompts: task.events.filter(e => task.active.currentPrompts?.includes(e.id))});
    } else res.json({desiredState: task.desiredState, session: task.session,
      prompts: task.active.state === 'Running' ? task.events.filter(e => task.active.currentPrompts?.includes(e.id)) : []});
  });
  app.post('/executions/:id/report', async (req, res) => {
    const task = await execution(req), executionId = task.active.id;
    const reportId = text(req.body.id, 200);
    await store.mutate(task.id, 'report:' + executionId + ':' + reportId, current => {
      if (current.active?.id !== executionId) throw new Problem(409, 'Stale execution');
      if (req.body.sessionId) current.session = {id: text(req.body.sessionId, 200), backend: current.active.model?.backend || 'claude', epoch: current.active.sessionEpoch};
      if (req.body.result) {
        const r = req.body.result;
        if (!Number.isInteger(r.exitCode) || typeof r.summary !== 'string') throw new Problem(400, 'Invalid result');
        current.active.result = {exitCode: r.exitCode, summary: r.summary.slice(0, 64000), sessionId: r.sessionId,
          usage: r.usage, cumulativeCostUSD: r.cumulativeCostUSD};
      } else if (req.body.summary) activity(current, agentActor(executionId),
        text(req.body.summary, 8000), 'controller', {kind: 'progress', executionId});
    });
    res.json({ok: true});
  });
  // Namespace facts and task reporting are execution-bound; outcomes remain controller-owned.
  app.post('/executions/:id/mcp', async (req, res) => {
    const task = await execution(req), body = req.body;
    let result;
    if (body.method === 'initialize') result = agentMcpInitialize();
    else if (body.method === 'tools/list') result = {tools: [{name: 'get_task_context',
      description: 'Read current task controls, reference draft and communication priority, including the primary GitHub issue URL. Use before governed actions and when choosing where to communicate; values may change during the turn.',
      inputSchema: {type: 'object', properties: {}, additionalProperties: false}},
      {name: 'declare_context_controls', description: 'Declare named on/off controls explicitly requested by the user. Adds new controls only; never overrides existing user settings. IDs, labels and descriptions are task-specific, not a fixed tests/CI list.',
        inputSchema: {type: 'object', properties: {controls: {type: 'object', additionalProperties: {type: 'object', properties: {
          label: {type: 'string'}, description: {type: 'string'}, enabled: {type: 'boolean'}}, required: ['label']}}}, required: ['controls'], additionalProperties: false}},
      {name: 'get_namespace_context',
      description: 'Discover this worker namespace’s provisioned container build configuration: registry address, Kaniko push Secret name/key, and Skaffold settings. Returns references only, never credentials. Namespace is fixed by the controller; no cross-namespace arguments.',
      inputSchema: {type: 'object', properties: {}, additionalProperties: false}}, {name: 'report_progress',
      description: 'Report progress or a question to the UI timeline. For an issue-backed task, publish blockers and confirmation questions to the primary GitHub issue as well. Does not post GitHub comments or mark completion.',
      inputSchema: {type: 'object', properties: {summary: {type: 'string'}}, required: ['summary']}},
      {name:'report_blocked',description:'Record that this turn is waiting for clarification or confirmation and retain the worker for follow-ups. For a GitHub issue task, first post the blocker and confirmation question to its primary issue; this records the blocker in the UI only. Include any comment-delivery failure in the summary. End this turn without further work and wait for a reply.',
        inputSchema:{type:'object',properties:{summary:{type:'string'}},required:['summary']}},
      {name: 'get_skaffold_schema', description: 'Read a named schema definition from the pinned runner Skaffold CLI. Start with SkaffoldConfig; use referencedDefinitions to retrieve ClusterDetails, DockerConfig, KanikoArtifact, RenderConfig or KubectlDeploy. No Kubernetes access.',
        inputSchema: {type: 'object', properties: {definition: {type: 'string'}}, additionalProperties: false}},
      {name: 'validate_skaffold_config', description: 'Validate one or more Skaffold YAML documents offline against the runner CLI schema. Returns invalid/unknown fields before any build or deployment. Does not execute commands or check RBAC/admission.',
        inputSchema: {type: 'object', properties: {content: {type: 'string', maxLength: 64000}}, required: ['content'], additionalProperties: false}},
      {name: 'get_skaffold_example', description: 'Get a schema-valid minimal Kaniko/Skaffold config using this namespace’s discovered push Secret. Uses manifests.rawYaml, the prepared namespace-qualified kubeconfig and preconfigured Skaffold environment variables. Push permission must still be checked through Driftmower.',
        inputSchema: {type: 'object', properties: {image: {type: 'string'}}, additionalProperties: false}},
      {name: 'register_github_pull_request', description: 'Associate a created or related GitHub PR with this Task, including a different repository. The controller verifies access and wakes this same Task when it merges; no new Task is created.',
        inputSchema: {type: 'object', properties: {url: {type: 'string'}}, required: ['url'], additionalProperties: false}},
      {name: 'send_email', description: 'Send a plain-text email for the current Task through the controller SMTP account when the Task authorizes email. Replies wake this same Task through IMAP. Use a stable idempotencyKey for each intended email; retry with the same key and content. Queued does not mean delivered. replyToMessageId must belong to this Task.',
        inputSchema: {type: 'object', properties: {to: {type: 'array', items: {type: 'string'}, minItems: 1, maxItems: 10},
          subject: {type: 'string', maxLength: 200}, text: {type: 'string', maxLength: 16000}, idempotencyKey: {type: 'string', maxLength: 200},
          replyToMessageId: {type: 'string'}}, required: ['to', 'subject', 'text', 'idempotencyKey'], additionalProperties: false}},
      {name: 'get_email_messages', description: 'Read email messages already routed to the current Task, including incoming replies and sent messages, plus live controller IMAP/SMTP configuration status. No unrelated inbox mail or other Tasks. Use this and get_email_status when asked about mail; do not infer live configuration from repository manifests.',
        inputSchema: {type: 'object', properties: {}, additionalProperties: false}},
      {name: ‘get_email_status’, description: ‘Read live controller IMAP/SMTP status and this Task’s outbound email delivery status. Credentials and other Tasks are not exposed.’,
        inputSchema: {type: ‘object’, properties: {}, additionalProperties: false}},
      ...(task.devices?.length ? [
        {name: ‘device_shell’, description: ‘Run a shell command on a connected device included in this task. Returns stdout, stderr and exit_code.’,
          inputSchema: {type: ‘object’, properties: {device_id: {type: ‘string’}, command: {type: ‘string’}}, required: [‘device_id’, ‘command’], additionalProperties: false}},
        {name: ‘device_file_read’, description: ‘Read a file from a connected device included in this task.’,
          inputSchema: {type: ‘object’, properties: {device_id: {type: ‘string’}, path: {type: ‘string’}}, required: [‘device_id’, ‘path’], additionalProperties: false}},
        {name: ‘device_file_write’, description: ‘Write content to a file on a connected device included in this task. Creates parent directories as needed.’,
          inputSchema: {type: ‘object’, properties: {device_id: {type: ‘string’}, path: {type: ‘string’}, content: {type: ‘string’}}, required: [‘device_id’, ‘path’, ‘content’], additionalProperties: false}},
      ] : [])]};

    else if (body.method === 'tools/call' && body.params?.name === 'get_task_context') {
      const value = {controls: task.controls || {}, draft: task.draft ? {title: task.draft.title, path: '/execution/draft.md'} : null,
        identity: config.identity, namespace: config.namespace, communication: taskCommunication(task),
        repair: repairContext(task),
        email: mailbox.context()};
      result = {content: [{type: 'text', text: JSON.stringify(value)}], structuredContent: value};
    } else if (body.method === 'tools/call' && body.params?.name === 'send_email') {
      const value = await mailbox.queue(task, agentActor(task.active.id), body.params.arguments || {});
      result = {content: [{type: 'text', text: JSON.stringify(value)}], structuredContent: value};
    } else if (body.method === 'tools/call' && body.params?.name === 'get_email_status') {
      const value = await mailbox.taskStatus(task.id);
      result = {content: [{type: 'text', text: JSON.stringify(value)}], structuredContent: value};
    } else if (body.method === 'tools/call' && body.params?.name === 'get_email_messages') {
      if (Object.keys(body.params.arguments || {}).length) throw new Problem(400, 'Task email messages take no arguments');
      const value = await mailbox.taskMessages(task.id);
      result = {content: [{type: 'text', text: JSON.stringify(value)}], structuredContent: value};
    } else if (body.method === 'tools/call' && body.params?.name === 'register_github_pull_request') {
      const reference = pullRequestReference(body.params.arguments?.url);
      if (!reference) throw new Problem(400, 'Expected GitHub pull request URL');
      const pull = await githubPullRequest(reference.url);
      await store.mutate(task.id, 'register-pr:' + reference.url, current => {
        if (current.active?.id !== task.active.id) throw new Problem(409, 'Stale execution');
        if (!current.references.some(value => value.kind === 'pr' && value.url === reference.url)) {
          current.references.push({...reference, observedAt: new Date().toISOString(), state: pull.merged ? 'merged' : pull.state,
            mergedAt: pull.mergedAt, mergeHandled: pull.merged, title: pull.title});
        }
      });
      result = {content: [{type: 'text', text: JSON.stringify({...reference, state: pull.merged ? 'merged' : pull.state})}]};
    } else if (body.method === 'tools/call' && body.params?.name === 'declare_context_controls') {
      const controls = normalizeControls(body.params.arguments?.controls);
      const updated = await store.mutate(task.id, 'controls:' + randomUUID(), current => {
        if (current.active?.id !== task.active.id) throw new Problem(409, 'Stale execution');
        current.controls = {...controls, ...(current.controls || {})};
        current.controls = normalizeControls(current.controls);
      });
      result = {content: [{type: 'text', text: JSON.stringify(updated.controls)}], structuredContent: {controls: updated.controls}};
    } else if (body.method === 'tools/call' && body.params?.name === 'get_namespace_context') {
      if (Object.keys(body.params.arguments || {}).length) throw new Problem(400, 'Namespace context takes no arguments');
      const context = await builds.list();
      result = {content: [{type: 'text', text: JSON.stringify(context)}], structuredContent: context};
    } else if (body.method === 'tools/call' && ['get_skaffold_schema', 'validate_skaffold_config', 'get_skaffold_example'].includes(body.params?.name)) {
      const args = body.params.arguments || {};
      const value = body.params.name === 'get_skaffold_schema' ? await skaffold.get(args.definition)
        : body.params.name === 'validate_skaffold_config' ? await skaffold.validate(args.content)
        : await skaffold.example(await builds.list(), args.image);
      result = {content: [{type: 'text', text: JSON.stringify(value)}], structuredContent: value};
    } else if (body.method === 'tools/call' && body.params?.name === 'report_progress') {
      await store.mutate(task.id, 'mcp:' + randomUUID(), current => {
        if (current.active?.id !== task.active.id) throw new Problem(409, 'Stale execution');
        activity(current, agentActor(task.active.id),
          text(body.params.arguments?.summary, 8000), 'controller', {kind: 'progress', executionId: task.active.id});
      });
      result = {content: [{type: 'text', text: 'Progress recorded'}]};
    } else if (body.method === 'tools/call' && body.params?.name === 'report_blocked') {
      await store.mutate(task.id,'blocked:'+randomUUID(),current=>blockExecution(current,task.active.id,body.params.arguments?.summary));
      result = {content:[{type:'text',text:'Blocker recorded. End this turn and wait for a reply. The worker stays warm for the idle timeout; this is not approval to proceed.'}]};
    } else if (body.method === 'tools/call' && ['device_shell', 'device_file_read', 'device_file_write'].includes(body.params?.name)) {
      const args = body.params.arguments || {};
      const key = args.device_id;
      if (!task.devices?.includes(key)) throw new Problem(400, 'Device not included in this task');
      const cmd = {device_shell: 'shell', device_file_read: 'file_read', device_file_write: 'file_write'}[body.params.name];
      const value = await devices.exec(key, cmd, args);
      result = {content: [{type: 'text', text: JSON.stringify(value)}], structuredContent: value};
    } else if (body.method?.startsWith('notifications/')) { res.status(202).end(); return; }
    else { res.json({jsonrpc: '2.0', id: body.id, error: {code: -32601, message: 'Unknown method'}}); return; }
    res.json({jsonrpc: '2.0', id: body.id, result});
  });

  let githubLogin;
  async function githubIdentity() {
    if (githubLogin) return githubLogin;
    const response = await fetch('https://api.github.com/user', {
      headers: {Authorization: `Bearer ${process.env.GH_TOKEN}`, Accept: 'application/vnd.github+json'},
      signal: AbortSignal.timeout(10000)});
    if (!response.ok) throw new Problem(400, 'Configured GitHub user credential is unavailable');
    githubLogin = (await response.json()).login;
    return githubLogin;
  }
  async function githubIssue(url) {
    const match = /^https:\/\/github\.com\/([\w.-]+)\/([\w.-]+)\/issues\/(\d+)$/.exec(url || '');
    if (!match) throw new Problem(400, 'Expected GitHub issue URL');
    const repository = `${match[1]}/${match[2]}`;
    const allow = (process.env.GIT_ALLOWED_REPOSITORIES || '').split(',').filter(Boolean);
    if (allow.length && !allow.includes(repository)) throw new Problem(403, 'Repository not authorized');
    const response = await fetch(`https://api.github.com/repos/${repository}/issues/${match[3]}`, {
      headers: {Authorization: `Bearer ${process.env.GH_TOKEN}`, Accept: 'application/vnd.github+json'},
      signal: AbortSignal.timeout(10000)});
    if (!response.ok) throw new Problem(response.status, 'Issue is inaccessible to the configured GitHub user');
    const issue = await response.json();
    return {reference: {source: 'github', repository, kind: 'issue', id: match[3], url},
      repository, state: issue.state, closedAt: issue.closed_at,
      updatedAt: issue.updated_at, assignees: (issue.assignees || []).map(user => user.login), isPullRequest: Boolean(issue.pull_request),
      title: issue.title,
      content: `${issue.title}\n\n${issue.body || ''}`};
  }
  async function repositoryAssignedIssues(repository, login) {
    const allow = (process.env.GIT_ALLOWED_REPOSITORIES || '').split(',').filter(Boolean);
    if (allow.length && !allow.includes(repository)) throw new Problem(403, 'Repository not authorized');
    const issues = [];
    for (let page = 1; page <= 10; page++) {
      const response = await fetch(`https://api.github.com/repos/${repository}/issues?assignee=${encodeURIComponent(login)}&state=open&per_page=100&page=${page}`, {
        headers: {Authorization: `Bearer ${process.env.GH_TOKEN}`, Accept: 'application/vnd.github+json'},
        signal: AbortSignal.timeout(10000), redirect: 'error'});
      if (!response.ok) throw new Problem(response.status, 'Repository issue lookup unavailable');
      const entries = await response.json();
      for (const issue of entries.filter(value => !value.pull_request)) issues.push({repository, state: issue.state,
        title: issue.title,
        closedAt: issue.closed_at, updatedAt: issue.updated_at, assignees: (issue.assignees || []).map(user => user.login),
        content: `${issue.title}\n\n${issue.body || ''}`, reference: {source: 'github', repository, kind: 'issue',
          id: String(issue.number), url: `https://github.com/${repository}/issues/${issue.number}`}});
      if (entries.length < 100) break;
    }
    return issues;
  }
  async function handleGithubMail(mail, delivery, options = {}) {
    const reference = githubNotificationReference(mail);
    if (!reference) return {outcome: 'ignored', reason: 'no_github_reference'};
    const login = await githubIdentity();
    if (reference.kind === 'pr') {
      const ids = await reconcileGithubPullRequests(store, githubPullRequest, login, reference.url);
      return {outcome: ids.length ? 'resumed' : 'ignored', taskIds: ids};
    }
    const issues = await resolveGithubMailIssues(reference, login, githubIssue, repositoryAssignedIssues);
    const outcomes = [];
    for (const issue of issues) outcomes.push(await routeGithubIssueMail(store, config.namespace, issue, login,
      mail.messageId || 'imap:' + delivery, options));
    return {outcome: outcomes.some(value => ['created', 'resumed', 'queued', 'completed'].includes(value.outcome)) ? 'routed' : 'ignored',
      repository: reference.repository, issue: reference.id, taskIds: outcomes.filter(value => value.taskId).map(value => value.taskId)};
  }
  async function githubPullRequest(url) {
    const reference = pullRequestReference(url);
    if (!reference) throw new Problem(400, 'Expected GitHub pull request URL');
    const allow = (process.env.GIT_ALLOWED_REPOSITORIES || '').split(',').filter(Boolean);
    if (allow.length && !allow.includes(reference.repository)) throw new Problem(403, 'Repository not authorized');
    const response = await fetch(`https://api.github.com/repos/${reference.repository}/pulls/${reference.id}`, {
      headers: {Authorization: `Bearer ${process.env.GH_TOKEN}`, Accept: 'application/vnd.github+json'},
      signal: AbortSignal.timeout(10000), redirect: 'error'});
    if (!response.ok) throw new Problem(502, 'GitHub PR lookup failed');
    const pull = await response.json();
    return {title: pull.title, state: pull.state, author: pull.user?.login, merged: Boolean(pull.merged), mergedAt: pull.merged_at};
  }
  app.post('/hooks/github', async (req, res) => {
    const signature = req.headers['x-hub-signature-256'];
    const expected = 'sha256=' + createHmac('sha256', process.env.GITHUB_WEBHOOK_SECRET || '').update(req.rawBody).digest('hex');
    if (!process.env.GITHUB_WEBHOOK_SECRET || !tokenMatches(signature, expected)) {
      res.locals.webhookReason = 'invalid_signature'; throw new Problem(401, 'Invalid GitHub signature');
    }
    res.locals.webhookAuthenticated = true;
    const delivery = text(req.headers['x-github-delivery'], 200);
    const eventType = req.headers['x-github-event'];
    if (eventType === 'ping') { res.locals.webhookOutcome = 'ping'; res.json({ok: true}); return; }
    if (eventType === 'pull_request') {
      if (req.body.action !== 'closed' || !req.body.pull_request?.merged) {
        res.locals.webhookOutcome = 'ignored'; res.locals.webhookReason = 'not_merged'; res.status(202).json({ignored: true}); return;
      }
      const reference = pullRequestReference(req.body.pull_request.html_url);
      if (!reference) throw new Problem(400, 'Expected GitHub PR reference');
      const ids = await reconcileGithubPullRequests(store, githubPullRequest, await githubIdentity(), reference.url);
      res.locals.webhookTaskIds = ids; res.locals.webhookOutcome = ids.length ? 'resumed' : 'ignored';
      if (!ids.length) res.locals.webhookReason = 'no_new_linked_merge';
      res.status(202).json({taskIds: ids}); return;
    }
    if (!['issues', 'issue_comment'].includes(eventType) || !req.body.issue || req.body.issue.pull_request) {
      res.locals.webhookOutcome = 'ignored'; res.locals.webhookReason = 'unsupported_event';
      res.status(202).json({ignored: true}); return;
    }
    if (eventType === 'issues' && req.body.action === 'closed') {
      const issue = await githubIssue(req.body.issue.html_url);
      const mapped = await redis.get(store.prefix + 'external:' + digest(`github:${issue.repository}:${issue.reference.id}`));
      if (!mapped || issue.state !== 'closed') {
        res.locals.webhookOutcome = 'ignored'; res.locals.webhookReason = !mapped ? 'no_existing_task' : 'issue_not_closed';
        res.status(202).json({ignored: true}); return;
      }
      await store.mutate(mapped, 'github:' + delivery, current => closeGithubTask(current,
        {type: 'external', provider: 'github', subject: String(req.body.sender?.id || ''), name: req.body.sender?.login || 'GitHub'}, issue.reference.url));
      res.locals.webhookTaskId = mapped; res.locals.webhookOutcome = 'completed';
      res.status(202).json({taskId: mapped}); return;
    }
    const login = await githubIdentity();
    if (!acceptsGithubEvent(eventType, req.body, login)) {
      res.locals.webhookOutcome = 'ignored'; res.locals.webhookReason = !req.body.issue.assignees?.some(a => a.login === login)
        ? 'not_assigned_to_agent' : req.body.sender?.login === login ? 'agent_authored_event' : 'unsupported_action';
      res.status(202).json({ignored: true}); return;
    }
    const issue = await githubIssue(req.body.issue.html_url);
    if (issue.state === 'closed') {
      res.locals.webhookOutcome = 'ignored'; res.locals.webhookReason = 'issue_closed';
      res.status(202).json({ignored: true}); return;
    }
    const key = `github:${issue.repository}:${issue.reference.id}`;
    const context = req.body.comment?.body || issue.content;
    const proposed = githubTask(config.namespace, {type: 'external', provider: 'github',
      subject: String(req.body.sender?.id || ''), name: req.body.sender?.login || 'GitHub'}, issue, eventType, context, delivery);
    proposed.githubIssueUpdatedAt = issue.updatedAt;
    const task = await store.create(proposed, 'external:' + key);
    res.locals.webhookTaskId = task.id;
    res.locals.webhookOutcome = task.id === proposed.id ? 'created'
      : task.events.some(e => e.deliveryId === delivery) ? 'duplicate' : 'resumed';
    await redis.set(store.prefix + 'external:' + digest(key), task.id);
    await store.mutate(task.id, 'github:' + delivery, current => {
      if (eventType === 'issues' && req.body.action === 'reopened') reopenGithubTask(current,
        {type: 'external', provider: 'github', subject: String(req.body.sender?.id || ''), name: req.body.sender?.login || 'GitHub'}, issue.reference.url);
      if (current.desiredState === 'Cancelled' || current.phase === 'Completed') return;
      if (current.events.some(e => e.deliveryId === delivery)) return;
      current.githubIssueUpdatedAt = issue.updatedAt;
      const source = activity(current, proposed.creator, text(context), 'webhook', {kind: 'event', deliveryId: delivery});
      prompt(current, controllerActor, githubWakeup(eventType, issue.reference.url), 'controller', {causedBy: [source.id]});
    });
    res.status(202).json({taskId: task.id});
  });
  app.post('/hooks/wakeup', async (req, res) => {
    if (!tokenMatches(req.headers.authorization?.replace(/^Bearer /, ''), process.env.WEBHOOK_TOKEN)) {
      res.locals.webhookReason = 'invalid_credential'; throw new Problem(401, 'Invalid webhook credential');
    }
    res.locals.webhookAuthenticated = true;
    const delivery = text(req.body.deliveryId, 200), context = text(req.body.context || 'Follow up on the current task.');
    let task;
    if (req.body.taskId) task = await store.get(id(req.body.taskId));
    else {
      const issue = await githubIssue(req.body.issueURL);
      const key = `github:${issue.repository}:${issue.reference.id}`;
      const mapped = await redis.get(store.prefix + 'external:' + digest(key));
      if (mapped) task = await store.get(mapped);
      else {
        const proposed = newTask(config.namespace, {type: 'external', name: 'Authenticated webhook'}, issue.content, 'webhook', [issue.reference]);
        proposed.repository = issue.repository;
        task = await store.create(proposed, 'external:' + key);
        await redis.set(store.prefix + 'external:' + digest(key), task.id);
      }
    }
    const updated = await store.mutate(task.id, 'webhook:' + delivery, current => {
      const source = activity(current, {type: 'external', name: 'Authenticated webhook'}, context, 'webhook', {kind: 'event'});
      prompt(current, controllerActor, `Process webhook follow-up:\n${context}`, 'controller', {causedBy: [source.id]});
    });
    res.locals.webhookTaskId = updated.id;
    res.status(202).json({taskId: updated.id});
  });
  app.post('/hooks/alertmanager', async (req, res) => {
    if (!tokenMatches(req.headers.authorization?.replace(/^Bearer /, ''), process.env.ALERTMANAGER_TOKEN)) {
      res.locals.webhookReason = 'invalid_credential'; throw new Problem(401, 'Invalid alert receiver credential');
    }
    res.locals.webhookAuthenticated = true;
    if (!Array.isArray(req.body.alerts) || req.body.alerts.length > 100) throw new Problem(400, 'Invalid alert batch');
    const ids = [];
    for (const alert of req.body.alerts) {
      const fingerprint = text(alert.fingerprint, 200), status = text(alert.status, 20);
      const key = 'alert:' + fingerprint + ':' + (alert.startsAt ? text(alert.startsAt, 100) : 'unspecified');
      const content = `Investigate ${status} alert ${alert.labels?.alertname || fingerprint}.\n${JSON.stringify({labels: alert.labels, annotations: alert.annotations})}`;
      const proposed = newTask(config.namespace, {type: 'external', name: 'Alertmanager'}, text(content),
        'alertmanager', [{source: 'alertmanager', fingerprint, startsAt: alert.startsAt}]);
      const task = await store.create(proposed, key);
      await store.mutate(task.id, 'alert:' + digest(JSON.stringify(alert)), current => {
        if (current.desiredState === 'Cancelled' || current.phase === 'Completed') return;
        const event = activity(current, {type: 'external', name: 'Alertmanager'}, content, 'alertmanager', {kind: 'event'});
        prompt(current, controllerActor, content, 'controller', {causedBy: [event.id]});
      });
      ids.push(task.id);
    }
    res.locals.webhookTaskIds = ids;
    res.status(202).json({taskIds: ids});
  });
  app.use(express.static('public'));
  app.get('/auth/callback', (req, res) => res.sendFile('public/index.html', {root: process.cwd()}));
  app.use((error, req, res, next) => {
    // Parser/upstream error messages may contain submitted payload fragments.
    // Webhook middleware already emits a content-free completion record.
    if (!(error instanceof Problem) && !req.webhookLogged) console.error(JSON.stringify({level: 'error', message: error.message}));
    res.status(error.status || 500).json({error: error instanceof Problem ? error.message : 'Request failed'});
  });
  let busy = false;
  let nextGithubSync = 0;
  config.syncGithub = async () => {
    if (Date.now() < nextGithubSync) return;
    nextGithubSync = Date.now() + 60000;
    await reconcileGithubIssues(store, githubIssue);
    await reconcileGithubPullRequests(store, githubPullRequest, await githubIdentity());
  };
  const tick = async () => {
    if (busy) return;
    busy = true;
    try { await reconcile(store, kube, config); }
    catch (error) {
      console.error(JSON.stringify({level: 'error', component: 'reconcile', message: error.message}));
      void faults.report(error, 'reconcile');
    }
    finally { busy = false; }
  };
  const interval = setInterval(tick, 5000);
  void tick();
  const cert = process.env.TLS_CERT_FILE, key = process.env.TLS_KEY_FILE;
  const server = cert ? createServer({cert: await readFile(cert), key: await readFile(key)}, app)
    : (await import('node:http')).createServer(app);
  server.listen(Number(process.env.PORT || 8080), '::', () => console.log(JSON.stringify({event: 'ready', namespace: config.namespace})));
  const wss = new WebSocketServer({noServer: true});
  server.on('upgrade', (req, socket, head) => {
    const match = /^\/devices\/([a-f0-9]{32})$/.exec(new URL(req.url, 'http://x').pathname);
    if (!match) { socket.destroy(); return; }
    wss.handleUpgrade(req, socket, head, ws => devices.handleConnection(match[1], ws));
  });
  process.on('SIGTERM', () => {
    clearInterval(interval);
    mailbox.close();
    server.close(async () => { await redis.quit(); process.exit(0); });
    setTimeout(() => process.exit(1), 25000).unref();
  });
}
