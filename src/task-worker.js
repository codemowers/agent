import amqp from 'amqplib';
import {spawn} from 'node:child_process';
import {randomUUID} from 'node:crypto';
import {mkdir, readFile, writeFile, appendFile, rename, glob} from 'node:fs/promises';
import {join} from 'node:path';
import {createInterface} from 'node:readline';
import {agentIdentity} from './identity.js';
import {signalChild} from './child-signals.js';
import {workerMcpConfig} from './mcp-auth.js';

const required = name => {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  return value;
};

function run(command, args, options = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, {stdio: ['ignore', 'pipe', 'pipe'], ...options});
    let stderr = '';
    child.stderr.on('data', chunk => { stderr = (stderr + chunk).slice(-2000); });
    child.on('error', reject);
    child.on('close', code => code === 0 ? resolve() : reject(new Error(`${command} exited ${code}: ${stderr}`)));
  });
}

async function workspace(root, taskName) {
  const repo = join(root, 'repo');
  await mkdir(join(root, 'home'), {recursive: true});
  await mkdir(join(root, 'executions'), {recursive: true});
  const identity = agentIdentity();
  await run('git', ['config', '--global', 'user.name', identity.name]);
  await run('git', ['config', '--global', 'user.email', identity.email]);
  await run('git', ['config', '--global', 'pull.rebase', 'true']);
  try { await readFile(join(repo, '.git', 'HEAD')); }
  catch {
    const repository = process.env.AGENT_REPOSITORY;
    const forgeURL = process.env.AGENT_FORGE_URL;
    if (repository && forgeURL) {
      if (!/^[\w.-]+\/[\w.-]+$/.test(repository)) throw new Error('Invalid Task repository');
      const server = new URL(forgeURL);
      if (!['https:', 'http:'].includes(server.protocol) || server.username || server.password) {
        throw new Error('Invalid configured Git server URL');
      }
      if (process.env.GH_TOKEN) {
        const username = process.env.AGENT_FORGE_PROVIDER === 'gitea' ? identity.username : 'x-access-token';
        process.env.GIT_CONFIG_COUNT = '1';
        process.env.GIT_CONFIG_KEY_0 = `http.${server.origin}/.extraHeader`;
        process.env.GIT_CONFIG_VALUE_0 = `Authorization: Basic ${Buffer.from(`${username}:${process.env.GH_TOKEN}`).toString('base64')}`;
      }
      await run('git', ['clone', `${forgeURL.replace(/\/$/, '')}/${repository}.git`, repo]);
    } else {
      await run('git', ['init', '--initial-branch=main', repo]);
    }
    await run('git', ['-C', repo, 'switch', '-c', `task/${taskName}`]);
  }
  return repo;
}

export async function seedTaskContext(repo, message) {
  const taskName = required('TASK_NAME');
  const spec = `specs/task-${taskName}.md`;
  await mkdir(join(repo, 'specs'), {recursive: true});
  if (message.type === 'objective') {
    await writeFile(join(repo, spec), `# Task objective\n\n${message.body}\n`, {flag: 'wx'}).catch(error => {
      if (error.code !== 'EEXIST') throw error;
    });
  }
  const instructions = `# Task workspace\n\n`
    + `Agent: ${required('AGENT_NAME')}\n\n`
    + `Read ${spec} when it exists, inspect the repository, and keep progress.md current.\n`
    + `Work on a topic branch. Do not push directly to main/master or force-push.\n`
    + `Verify build and deployment settings from the available MCPs before infrastructure actions.\n`
    + `Do not improvise builders, registries, credentials or namespace privileges when infrastructure is unclear.\n`;
  await writeFile(join(repo, 'AGENTS.md'), instructions, {flag: 'wx'}).catch(error => {
    if (error.code !== 'EEXIST') throw error;
  });
}

export async function nativeSessionExists(root, backend, sessionId) {
  if (!sessionId) return false;
  const home = join(root, 'home');
  const pattern = backend === 'claude' ? `${home}/.claude/projects/*/${sessionId}.jsonl`
    : `${home}/.codex/sessions/**/*${sessionId}*.jsonl`;
  for await (const _file of glob(pattern)) return true;
  return false;
}

async function runPrompt(message, state, root, repo, backend, model, signal) {
  await seedTaskContext(repo, message);
  const endpoints = JSON.parse(process.env.AGENT_MCP_ENDPOINTS || '[]');
  const mcpServers = {};
  for (const [index, endpoint] of endpoints.entries()) {
    const name = endpoint.name || `mcp${index + 1}`;
    if (!/^[a-zA-Z0-9_-]+$/.test(name) || mcpServers[name] || !endpoint.url) {
      throw new Error('Invalid or duplicate Agent MCP endpoint');
    }
    mcpServers[name] = {type: 'http', url: endpoint.url,
      ...(endpoint.tokenFile ? {tokenFile: endpoint.tokenFile} : {})};
  }
  const mcp = await workerMcpConfig({mcpServers});
  const startedAt = new Date().toISOString();
  const id = randomUUID();
  const logPath = join(root, 'executions', `${id}.jsonl`);
  const prompt = `Task ${required('TASK_NAMESPACE')}/${required('TASK_NAME')} for Agent ${required('AGENT_NAME')}.\n`
    + `Read the workspace instructions and continue the Task.\n`
    + (process.env.AGENT_PREVIOUS_SUMMARY ? `Previous worker summary:\n${process.env.AGENT_PREVIOUS_SUMMARY}\n\n` : '\n')
    + message.body;
  const sessionId = state.sessionId || randomUUID();
  const sessionPath = join(root, 'session.json');
  const recordSession = async nativeId => writeFile(sessionPath, JSON.stringify({
    sessionId: nativeId, backend, model, epoch: state.epoch || 0,
  }), {mode: 0o600});
  let args;
  if (backend === 'claude') {
    args = ['-p', prompt, '--output-format', 'stream-json', '--verbose', '--dangerously-skip-permissions',
      '--strict-mcp-config', '--mcp-config', JSON.stringify(mcp)];
    if (model) args.push('--model', model);
    args.push(state.sessionId ? '--resume' : '--session-id', sessionId);
    await recordSession(sessionId);
  } else {
    args = ['exec', '--json', '--dangerously-bypass-approvals-and-sandbox', '--skip-git-repo-check'];
    if (model) args.push('--model', model);
    for (const [name, server] of Object.entries(mcp.mcpServers)) {
      args.push('-c', `mcp_servers.${JSON.stringify(name)}.url=${JSON.stringify(server.url)}`);
    }
    if (state.sessionId) args.push('resume', state.sessionId);
    args.push(prompt);
  }
  const cliEnv = {...process.env, HOME: join(root, 'home')};
  if (backend === 'codex') cliEnv.CODEX_API_KEY = process.env.OPENAI_API_KEY;
  const child = spawn(backend, args, {cwd: repo, env: cliEnv, detached: true, stdio: ['ignore', 'pipe', 'pipe']});
  const interrupt = () => { if (child.exitCode === null) signalChild(child, 'SIGTERM'); };
  signal.addEventListener('abort', interrupt, {once: true});
  let summary = '', reportedSession = sessionId, nativeFailure = false;
  let stderr = '';
  child.stderr.on('data', chunk => { stderr = (stderr + chunk).slice(-2000); });
  const exited = new Promise((resolve, reject) => {
    child.on('error', reject);
    child.on('close', (code, reason) => resolve(code ?? (reason ? 143 : 1)));
  });
  try {
    for await (const line of createInterface({input: child.stdout})) {
      await appendFile(logPath, line + '\n', {mode: 0o600});
      let event;
      try { event = JSON.parse(line); } catch { continue; }
      if (backend === 'claude') {
        if (event.session_id && event.session_id !== reportedSession) {
          reportedSession = event.session_id;
          await recordSession(reportedSession);
        }
        if (event.type === 'result') {
          summary = event.result || '';
          nativeFailure = Boolean(event.is_error);
        }
      } else {
        if (event.type === 'thread.started' && event.thread_id) {
          reportedSession = event.thread_id;
          await recordSession(reportedSession);
        }
        if (event.type === 'item.completed' && event.item?.type === 'agent_message') summary = event.item.text || '';
        if (event.type === 'turn.failed') nativeFailure = true;
      }
    }
    const exitCode = await exited;
    const result = {id, startedAt, completedAt: new Date().toISOString(), exitCode: exitCode || (nativeFailure ? 1 : 0),
      sessionId: reportedSession, summary: summary.slice(0, 8000) || (stderr ? 'CLI failed; see private worker stderr.' : 'No final report')};
    await writeFile(join(root, 'executions', `${id}.result.json`), JSON.stringify(result), {mode: 0o600});
    console.log(JSON.stringify({type: 'agent_result', task: process.env.TASK_NAME, result}));
    return result;
  } finally {
    signal.removeEventListener('abort', interrupt);
    mcp.close();
  }
}

export async function taskWorker() {
  const backend = required('AGENT_BACKEND');
  if (!['claude', 'codex'].includes(backend)) throw new Error('Unsupported backend');
  const credential = backend === 'claude' ? 'ANTHROPIC_API_KEY' : 'OPENAI_API_KEY';
  required(credential);
  const queue = required('TASK_QUEUE');
  const root = process.env.AGENT_STATE_DIR || '/workspace';
  process.env.HOME = join(root, 'home');
  const repo = await workspace(root, required('TASK_NAME'));
  const statePath = join(root, 'session.json');
  const receipts = join(root, 'receipts');
  await mkdir(receipts, {recursive: true});
  let state = {};
  try { state = JSON.parse(await readFile(statePath, 'utf8')); }
  catch (error) { if (error.code !== 'ENOENT') throw error; }
  if (state.backend !== backend || state.model !== process.env.AGENT_MODEL) state = {};
  if (state.sessionId && !await nativeSessionExists(root, backend, state.sessionId)) {
    await appendFile(join(root, 'executions', 'session-resets.jsonl'), JSON.stringify({
      at: new Date().toISOString(), previousSessionId: state.sessionId,
      reason: 'Native session file missing; starting a new conversation epoch',
    }) + '\n', {mode: 0o600});
    state = {backend, model: process.env.AGENT_MODEL, epoch: (state.epoch || 0) + 1};
  }
  const controller = new AbortController();
  process.once('SIGTERM', () => controller.abort());
  process.once('SIGINT', () => controller.abort());
  const connection = await amqp.connect(required('AMQP_URL'));
  const channel = await connection.createChannel();
  await channel.prefetch(1);
  let finished = false;
  let skippedCompletedReceipt = false;
  let completedTurn = false;
  try {
    while (!controller.signal.aborted && !finished) {
      const delivery = await channel.get(queue, {noAck: false});
      if (!delivery) {
        if (skippedCompletedReceipt || completedTurn) break;
        await new Promise(resolve => setTimeout(resolve, 2000));
        continue;
      }
      let message;
      try { message = JSON.parse(delivery.content.toString('utf8')); }
      catch { channel.nack(delivery, false, false); continue; }
      if (typeof message.body !== 'string' || !['objective', 'prompt', 'event'].includes(message.type)) {
        channel.nack(delivery, false, false);
        continue;
      }
      const messageId = typeof message.messageId === 'string' && /^[a-zA-Z0-9:._-]{1,160}$/.test(message.messageId)
        ? message.messageId : undefined;
      const receiptPath = messageId && join(receipts, messageId + '.json');
      if (receiptPath) {
        try {
          const receipt = JSON.parse(await readFile(receiptPath, 'utf8'));
          if (receipt.messageId === messageId && receipt.exitCode === 0) {
            channel.ack(delivery);
            skippedCompletedReceipt = true;
            continue;
          }
        } catch (error) { if (error.code !== 'ENOENT' && !(error instanceof SyntaxError)) throw error; }
      }
      const result = await runPrompt(message, state, root, repo, backend, process.env.AGENT_MODEL, controller.signal);
      if (controller.signal.aborted) break;
      if (result.exitCode !== 0) {
        channel.nack(delivery, false, false);
        process.exitCode = result.exitCode;
        finished = true;
      } else {
        state = {sessionId: result.sessionId, backend, model: process.env.AGENT_MODEL, epoch: state.epoch || 0};
        await writeFile(statePath, JSON.stringify(state), {mode: 0o600});
        if (receiptPath) {
          const tmp = receiptPath + '.' + randomUUID();
          await writeFile(tmp, JSON.stringify({messageId, exitCode: 0}), {mode: 0o600});
          await rename(tmp, receiptPath);
        }
        channel.ack(delivery);
        completedTurn = true;
      }
      await writeFile('/dev/termination-log', JSON.stringify({summary: result.summary.slice(0, 3000),
        completedAt: result.completedAt, exitCode: result.exitCode})).catch(() => {});
    }
  } finally {
    await channel.close().catch(() => {});
    await connection.close().catch(() => {});
  }
}
