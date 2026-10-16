import {forgeConfiguration} from './forge.js';
import {mkdir, readFile, writeFile, appendFile, glob} from 'node:fs/promises';
import {spawn} from 'node:child_process';
import {createInterface} from 'node:readline';
import {randomUUID} from 'node:crypto';
import {setTimeout as delay} from 'node:timers/promises';
import {command} from './kube.js';
import {turnMetrics} from './metrics.js';
import {workerMcpConfig, readMcpConfig} from './mcp-auth.js';
import {toolEvents} from './tool-events.js';
import {mcpPayloadEnabled} from './mcp-payload.js';
import {watchTurnControl} from './turn-control.js';
import {initializeRepository, prepareWorkspace} from './workspace.js';
import {agentIdentity} from './identity.js';
import {signalChild} from './child-signals.js';
import {taskCommunication} from './agent-mcp.js';
import {agentFetch} from './http.js';

export async function worker(onException = async () => {}) {
  const root = process.env.AGENT_STATE_DIR || '/workspace';
  const base = process.env.AGENT_SERVICE_URL;
  const taskId = process.env.AGENT_TASK_ID, executionId = process.env.AGENT_EXECUTION_ID;
  const token = process.env.AGENT_REPORT_TOKEN;
  const endpoint = `${base}/executions/${taskId}`;
  let sequence = 0;
  async function emit(body) {
    const entry = {type: 'agent_event', executionId, sequence: ++sequence, ...body};
    const line = JSON.stringify(entry);
    await appendFile(`${root}/executions/${executionId}.events.jsonl`, line + '\n');
    console.log(line);
  }
  await mkdir(root + '/home', {recursive: true});
  await mkdir(root + '/executions', {recursive: true});
  let envelope = JSON.parse(await readFile(process.env.AGENT_ENVELOPE_FILE, 'utf8'));
  const backend = process.env.AGENT_BACKEND || 'claude';
  if (!['claude', 'codex'].includes(backend)) throw new Error('Unknown backend');
  const credential = backend === 'codex' ? 'OPENAI_API_KEY' : 'ANTHROPIC_API_KEY';
  if (!process.env[credential]?.trim()) {
    const result = {exitCode: 1, completedAt: new Date().toISOString(), sessionId: envelope.session?.id || envelope.sessionId,
      summary: `${backend === 'codex' ? 'OpenAI' : 'Anthropic'} API unavailable: ${credential} is not configured for this execution.`};
    await writeFile(`${root}/executions/${executionId}.result.json`, JSON.stringify(result), {mode: 0o600});
    console.log(JSON.stringify({type: 'agent_result', executionId, result}));
    await emit({kind: 'result', result});
    process.exitCode = result.exitCode;
    return;
  }
  let child, stopped = false;
  process.on('SIGTERM', () => {
    stopped = true;
    const target = child;
    signalChild(target, 'SIGTERM');
    setTimeout(() => signalChild(target, 'SIGKILL'), 30000).unref();
  });
  const cwd = root + '/repo';
  const forge = forgeConfiguration();
  if (process.env.GH_TOKEN && forge.provider === 'github') {
    if (forge.serverURL !== 'https://github.com') process.env.GH_ENTERPRISE_TOKEN = process.env.GH_TOKEN;
    await command('gh', ['auth', 'setup-git']);
  } else if (process.env.GH_TOKEN) {
    process.env.GIT_CONFIG_COUNT = '1';
    process.env.GIT_CONFIG_KEY_0 = 'http.' + forge.serverURL + '/.extraHeader';
    process.env.GIT_CONFIG_VALUE_0 = 'Authorization: Basic ' + Buffer.from(agentIdentity().username + ':' + process.env.GH_TOKEN).toString('base64');
  }
  await initializeRepository(cwd, envelope.repository);
  const mcpSource = await readMcpConfig();
  const mcp = await workerMcpConfig(mcpSource);
  mcp.mcpServers.agent = {type: 'http', url: endpoint + '/mcp', headers: {Authorization: `Bearer ${token}`}};
  const secrets = Object.entries(process.env).filter(([name])=>/TOKEN|PASSWORD|SECRET|API_KEY|GIT_CONFIG_VALUE/.test(name)).map(([,value])=>value);
  let turn = 0;
  async function runTurn(envelope) {
  let control, interruptKillTimer;
  try {
  const instructions = await readFile(process.env.AGENT_INSTRUCTIONS_FILE, 'utf8');
  let infrastructure, controls = {}, draft, identity = agentIdentity(), email, repair, devices = [], communication = taskCommunication(envelope);
  try {
    const response = await agentFetch(endpoint + '/context', {headers: {Authorization: `Bearer ${token}`},
      signal: AbortSignal.timeout(30000), redirect: 'error'});
    if (!response.ok) throw new Error('Build discovery unavailable');
    const context = await response.json();
    ({infrastructure, controls, draft, email, repair, devices = []} = context);
    identity = context.identity || identity;
    communication = context.communication || communication;
  } catch { infrastructure = {status: 'unavailable', detail: 'Controller build discovery unavailable; do not guess infrastructure configuration.'}; }
  const workspace = await prepareWorkspace(cwd, envelope, infrastructure, draft, mcpSource);
  const turnStarted = performance.now(), startedAt = new Date().toISOString();
  await emit({kind: 'turn_started', startedAt, supportsPromptStop: true});
  let resume = Boolean(envelope.session?.id);
  let sessionId = envelope.session?.id || envelope.sessionId;
  if (resume) {
    let found = false;
    const pattern = backend === 'codex' ? `${process.env.HOME}/.codex/sessions/**/*${sessionId}*.jsonl`
      : `${process.env.HOME}/.claude/projects/*/${sessionId}.jsonl`;
    for await (const file of glob(pattern)) { found = true; break; }
    if (!found) {
      resume = false; sessionId = randomUUID();
      await emit({kind: 'session_reset', sessionId, reason: 'Native conversation missing; starting a new session with the existing workspace and objective.'});
    }
  }
  const input = 'Current agent identity (controller-provided; repository templates are not live configuration):\n'
    + JSON.stringify(identity) + '\nGit provider (use gh for GitHub/GHE; the configured API for Gitea when GH_TOKEN is set):\n'
    + JSON.stringify({...forge, authenticated: Boolean(process.env.GH_TOKEN)}) + '\nTask communication priority:\n' + JSON.stringify(communication)
    + '\nController mailbox status:\n' + JSON.stringify(email || {status: 'unavailable', verifyWith: 'agent.get_email_status'}) + '\n\n'
    + 'Devices included in this Task (use these IDs with agent device tools; refresh with agent.get_task_context):\n'
    + JSON.stringify(devices) + '\n\n'
    + (repair ? 'Latest controller-deduplicated repair context:\n' + JSON.stringify(repair) + '\n\n' : '')
    + `Task objective:\n${envelope.objective}\n\n` + (!resume && envelope.previousSummary
    ? `Previous execution summary:\n${envelope.previousSummary}\nInspect the existing workspace and progress.md before continuing.\n\n` : '')
    + 'Controller-discovered namespace build configuration:\n' + JSON.stringify(infrastructure) + '\n\n'
    + 'Task context controls (user choices; obey the descriptions):\n' + JSON.stringify(controls) + '\n\n'
    + `Workspace context: ${JSON.stringify(workspace)}. Read AGENTS.md and the specification before planning; keep them updated as requirements change. The user's prompt supplies implementation authorization.\n\n`
    + 'New prompts:\n' + envelope.prompts.map(p =>
    `[${p.actor.type}: ${p.actor.name || p.actor.subject || 'unknown'}]\n${p.content}`).join('\n\n');
  let args = ['-p', input, '--output-format', 'stream-json', '--verbose',
    '--dangerously-skip-permissions', '--append-system-prompt', instructions,
    '--strict-mcp-config', '--mcp-config', JSON.stringify(mcp), '--model', process.env.AGENT_MODEL,
    '--max-budget-usd', process.env.AGENT_BUDGET_USD || '5'];
  args.push(resume ? '--resume' : '--session-id', sessionId);
  if (backend === 'codex') {
    args = ['exec', '--json', '--dangerously-bypass-approvals-and-sandbox', '--skip-git-repo-check',
      '-c', 'developer_instructions=' + JSON.stringify(instructions),
      '-c', 'shell_environment_policy.inherit="all"'];
    if (process.env.AGENT_MODEL) args.push('--model', process.env.AGENT_MODEL);
    for (const [name, server] of Object.entries(mcp.mcpServers)) {
      const prefix = 'mcp_servers.' + JSON.stringify(name);
      args.push('-c', prefix + '.url=' + JSON.stringify(server.url));
      if (server.headers) args.push('-c', prefix + '.http_headers={' + Object.entries(server.headers)
        .map(([key, value]) => JSON.stringify(key) + '=' + JSON.stringify(value)).join(',') + '}');
    }
    if (resume) args.push('resume', sessionId);
    args.push(input);
  }
  if (stopped) { await emit({kind: 'result', result: {exitCode: 143, summary: 'Stopped before CLI start'}}); return; }
  let interrupted = false;
  function interruptCLI() {
    if (child && (child.exitCode !== null || child.signalCode !== null)) return;
    interrupted = true;
    if (child?.pid) {
      const target = child;
      if (!signalChild(target, 'SIGINT')) console.warn(JSON.stringify({event: 'turn_control_signal_failed', signal: 'SIGINT'}));
      interruptKillTimer = setTimeout(() => {
        if (!signalChild(target, 'SIGKILL') && target.exitCode === null && target.signalCode === null)
          console.warn(JSON.stringify({event: 'turn_control_signal_failed', signal: 'SIGKILL'}));
      }, 10000);
      interruptKillTimer.unref();
    }
  }
  control = await watchTurnControl({endpoint, token, promptIds: envelope.prompts.map(prompt => prompt.id), interrupt: interruptCLI});
  if (interrupted) {
    const result = {exitCode: 130, interrupted: true, completedAt: new Date().toISOString(), sessionId,
      summary: 'Prompt stopped by user. Workspace and resident Pod retained.'};
    await writeFile(`${root}/executions/${executionId}.result.json`, JSON.stringify(result), {mode: 0o600});
    await emit({kind: 'turn_result', result}); return result;
  }
  await emit({kind: 'session', sessionId});
  const turnSuffix = turn++ ? `.turn-${turn}` : '';
  const logFile = `${root}/executions/${executionId}${turnSuffix}.jsonl`;
  const stderrFile = `${root}/executions/${executionId}${turnSuffix}.stderr`;
  let nativeResult, reportedSession = sessionId, nativeError = '';
  const cliEnv = {...process.env};
  // Refresh defaults for every native CLI turn, including resident workers.
  delete cliEnv.SKAFFOLD_DEFAULT_REPO;
  if (infrastructure?.status === 'available') cliEnv.SKAFFOLD_DEFAULT_REPO = infrastructure.skaffold.defaultRepo;
  cliEnv.SKAFFOLD_NAMESPACE = process.env.AGENT_NAMESPACE;
  if (infrastructure?.skaffold?.kubeContext) cliEnv.SKAFFOLD_KUBE_CONTEXT = infrastructure.skaffold.kubeContext;
  // codex exec accepts its automation API key through CODEX_API_KEY.
  if (backend === 'codex') cliEnv.CODEX_API_KEY = process.env.OPENAI_API_KEY;
  child = spawn(backend, args, {cwd, detached: true, stdio: ['ignore', 'pipe', 'pipe'], env: cliEnv});
  if (interrupted) interruptCLI();
  if (stopped) signalChild(child, 'SIGTERM');
  const exited = new Promise((resolve, reject) => {
    child.on('error', reject); child.on('close', (code, signal) => resolve(code ?? (signal ? 143 : 1)));
  });
  child.stderr.on('data', data => {
    nativeError = (nativeError + data.toString()).slice(-2000);
    // Persist diagnostics privately; never forward stderr into public task metadata.
    void appendFile(stderrFile, data);
  });
  let codexSummary = '', codexUsage, codexFailed = false, codexError = '';
  const mcpToolIds = new Set();
  for await (const line of createInterface({input: child.stdout})) {
    await appendFile(logFile, line + '\n');
    let event;
    try { event = JSON.parse(line); } catch (e) { if (e instanceof SyntaxError) continue; throw e; }
    if (mcpPayloadEnabled() && backend === 'claude' && event.type === 'assistant') {
      for (const block of event.message?.content || []) if (block.type === 'tool_use' && block.name?.startsWith('mcp__')) mcpToolIds.add(block.id);
    }
    for (const entry of toolEvents(backend,event,secrets,mcpPayloadEnabled(),mcpToolIds)) if (entry.toolId) {
      await emit(entry);
    }
    if (event.type === 'user') for (const block of event.message?.content || []) if (block.type === 'tool_result') mcpToolIds.delete(block.tool_use_id);
    if (backend === 'codex') {
      if (event.type === 'thread.started' && event.thread_id) {
        reportedSession = event.thread_id;
        await emit({kind: 'session', sessionId: reportedSession});
      }
      if (event.type === 'item.completed' && event.item?.type === 'agent_message') {
        codexSummary = event.item.text || '';
        if (codexSummary) await emit({kind: 'progress', summary: codexSummary.slice(0, 8000)});
      }
      if (event.type === 'turn.completed') { codexUsage = event.usage; codexFailed = false; }
      if (event.type === 'turn.failed') {
        codexFailed = true;
        codexError = (event.error?.message || 'Codex turn failed').replace(/sk-[A-Za-z0-9_*.\-]+/g, '[REDACTED]');
      }
      continue;
    }
    if (event.session_id && event.session_id !== reportedSession) {
      reportedSession = event.session_id;
      await emit({kind: 'session', sessionId: reportedSession});
    }
    if (event.type === 'assistant') {
      const summary = (event.message?.content || []).filter(c => c.type === 'text').map(c => c.text).join('\n').slice(0, 8000);
      if (summary) await emit({kind: 'progress', summary});
    }
    if (event.type === 'result') nativeResult = event;
  }
  const exitCode = await exited;
  child = null;
  const result = {exitCode: stopped ? 143 : (exitCode || (nativeResult?.is_error || codexFailed ? 1 : 0)),
    completedAt: new Date().toISOString(),
    sessionId: reportedSession,
    summary: nativeResult?.result || codexError || codexSummary || (stopped ? 'Execution interrupted.' : 'No final answer. Inspect worker diagnostics on the scratch PVC.'),
    usage: nativeResult?.usage || codexUsage, cumulativeCostUSD: nativeResult?.total_cost_usd,
    metrics: turnMetrics(Math.round(performance.now() - turnStarted), nativeResult?.usage || codexUsage, nativeResult?.duration_api_ms)};
  if (interrupted) Object.assign(result, {exitCode: 130, interrupted: true,
    summary: 'Prompt stopped by user. Workspace and resident Pod retained.'});
  await writeFile(`${root}/executions/${executionId}.result.json`, JSON.stringify(result), {mode: 0o600});
  await emit({kind: 'turn_result', result});
  return result;
  } finally {
    control?.close(); clearTimeout(interruptKillTimer);
    if (child?.pid && child.exitCode === null && child.signalCode === null) {
      signalChild(child, 'SIGKILL');
    }
    child = null;
  }
  }
  let idleSeconds = Number(process.env.AGENT_IDLE_TIMEOUT_SECONDS || 0);
  let result;
  let stopAfterTurn = false;
  while (!stopped) {
    try { result = await runTurn(envelope); }
    catch (error) {
      await onException(error, 'worker_turn');
      result = {exitCode: 1, completedAt: new Date().toISOString(), sessionId: envelope.session?.id || envelope.sessionId,
        summary: 'Worker turn failed due to an internal exception. Repair reporting was attempted; the resident Pod remains available for follow-up.'};
      await emit({kind: 'turn_result', result});
      idleSeconds ||= 3600;
    }
    if (result?.interrupted && !idleSeconds) idleSeconds = 3600;
    // A blocker can promote an event-driven worker to a resident session.
    // Refresh after the CLI exits so a fast final response cannot miss the update.
    if (result && !stopped) {
      try {
        const response = await agentFetch(endpoint + '/control', {headers: {Authorization: `Bearer ${token}`},
          signal: AbortSignal.timeout(3000), redirect: 'error'});
        if (response.ok) {
          const control = await response.json();
          if (Number.isInteger(control.idleTimeoutSeconds) && control.idleTimeoutSeconds > 0) idleSeconds = control.idleTimeoutSeconds;
          stopAfterTurn = control.stopAfterTurn === true;
        }
      } catch { /* Retain the established timeout during controller outages. */ }
    }
    if (!result || !idleSeconds || stopped || stopAfterTurn) break;
    const deadline = Date.parse(result.completedAt) + idleSeconds * 1000;
    const afterSequence = sequence, requestId = randomUUID();
    let next;
    while (!stopped && Date.now() < deadline) {
      await delay(2000);
      if (stopped || Date.now() >= deadline) break;
      try {
        const response = await agentFetch(endpoint + '/next', {method: 'POST',
          headers: {authorization: `Bearer ${token}`, 'content-type': 'application/json'},
          body: JSON.stringify({id: requestId, afterSequence}), signal: AbortSignal.timeout(10000)});
        if (!response.ok) continue;
        const inbox = await response.json();
        if (inbox.stopAfterTurn) { stopAfterTurn = true; break; }
        if (inbox.desiredState !== 'Active') { stopped = true; break; }
        if (inbox.prompts.length) { next = {...envelope, prompts: inbox.prompts, session: inbox.session}; break; }
      } catch { /* Keep the resident worker during transient controller outages. */ }
    }
    if (!next) break;
    envelope = next;
  }
  if (stopped) result = {...result, exitCode: 143, summary: 'Execution interrupted.'};
  result ||= {exitCode: 143, summary: 'Stopped before CLI start'};
  await writeFile(`${root}/executions/${executionId}.result.json`, JSON.stringify(result), {mode: 0o600});
  console.log(JSON.stringify({type: 'agent_result', executionId, result}));
  await emit({kind: 'result', result});
  mcp.close();
  process.exitCode = result.exitCode;
}
