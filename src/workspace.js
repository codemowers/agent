import {mkdir, readFile, writeFile, appendFile, lstat} from 'node:fs/promises';
import {join} from 'node:path';
import {command} from './kube.js';
import {agentIdentity} from './identity.js';

export async function initializeRepository(cwd, repository) {
  const identity = agentIdentity();
  for (const [key, value] of [['user.name', identity.name], ['user.email', identity.email], ['pull.rebase', 'true']]) {
    await command('git', ['config', '--global', key, value]);
  }
  try { await lstat(join(cwd, '.git')); return; }
  catch (error) { if (error.code !== 'ENOENT') throw error; }
  if (repository) {
    if (!/^[\w.-]+\/[\w.-]+$/.test(repository)) throw new Error('Invalid repository');
    await command('git', ['clone', `https://github.com/${repository}.git`, cwd]);
  } else {
    await mkdir(cwd, {recursive: true});
    await command('git', ['init', '--initial-branch=main', cwd]);
  }
}

async function seed(path, content) {
  try { await writeFile(path, content, {flag: 'wx'}); }
  catch (error) { if (error.code !== 'EEXIST') throw error; }
}

export async function prepareWorkspace(cwd, envelope, infrastructure, draft, mcpConfig) {
  const taskId = envelope.id || process.env.AGENT_TASK_ID;
  if (!/^[a-f0-9-]{36}$/.test(taskId || '')) throw new Error('Invalid workspace Task identity');
  const slug = draft?.id?.replace(/\.md$/, '').replace(/[^a-z0-9_-]/gi, '-').slice(0, 64);
  const specification = `specs/${slug ? slug + '-' : 'task-'}${taskId.slice(0, 8)}.md`;
  const content = draft ? await readFile(draft.path, 'utf8') : `# Task specification\n\n${envelope.objective}\n`;
  await mkdir(join(cwd, 'specs'), {recursive: true});
  await seed(join(cwd, specification), content);
  const namespace = infrastructure?.namespace || process.env.AGENT_NAMESPACE;
  const context = infrastructure?.skaffold?.kubeContext || process.env.AGENT_KUBE_CONTEXT;
  const defaults = {
    ...(infrastructure?.status === 'available' ? {SKAFFOLD_DEFAULT_REPO: infrastructure.skaffold.defaultRepo} : {}),
    ...(namespace ? {SKAFFOLD_NAMESPACE: namespace} : {}),
    ...(context ? {SKAFFOLD_KUBE_CONTEXT: context} : {}),
    KUBECONFIG: '/config/kubeconfig',
  };
  const env = '# Non-secret defaults; credentials remain in the mounted ServiceAccount kubeconfig.\n'
    + Object.entries(defaults).map(([key, value]) => `${key}=${JSON.stringify(value)}`).join('\n') + '\n';
  await seed(join(cwd, 'skaffold.env.example'), env);
  const envPath = join(cwd, 'skaffold.env');
  let previous = '';
  try { previous = await readFile(envPath, 'utf8'); }
  catch (error) { if (error.code !== 'ENOENT') throw error; }
  const owned = ['SKAFFOLD_DEFAULT_REPO', 'SKAFFOLD_NAMESPACE', 'SKAFFOLD_KUBE_CONTEXT', 'KUBECONFIG'];
  const retained = previous.split('\n').filter(line => !owned.some(key => new RegExp(`^\\s*(?:export\\s+)?${key}\\s*=`).test(line))).join('\n').trimEnd();
  await writeFile(envPath, (retained ? retained + '\n' : '') + Object.entries(defaults)
    .map(([key, value]) => `${key}=${JSON.stringify(value)}`).join('\n') + '\n', {mode: 0o600});
  const ignore = join(cwd, '.gitignore');
  await seed(ignore, '');
  if (!(await readFile(ignore, 'utf8')).split('\n').some(line => ['/skaffold.env', 'skaffold.env'].includes(line.trim()))) {
    await appendFile(ignore, '\n# Worker-specific Skaffold defaults\n/skaffold.env\n');
  }
  const references = Object.entries(mcpConfig.mcpServers || {}).flatMap(([name, server]) => {
    if (!server.url) return [];
    const url = new URL(server.url);
    return [`- ${name}: ${url.origin + url.pathname}`];
  });
  const marker = `<!-- codemowers/agent task ${taskId} -->`;
  const instructions = `${marker}\n## Agent workspace context\n\n`
    + `- Read [the task specification](${specification}) before planning. Keep it updated when the user changes requirements.\n`
    + '- Read [Skaffold defaults](skaffold.env). Use the worker environment and existing kubeconfig; credentials are mounted separately.\n'
    + '- MCP scopes: codemowers for engineering guidance, driftmower for cluster capabilities/inventory/logs/metrics, agent for namespace configuration and current task controls.\n'
    + references.join('\n') + '\n'
    + '- Use driftmower.query_logs for application/build logs before kubectl logs. Query admission policies and resource schemas before platform manifests.\n'
    + '- Validate skaffold.yaml with agent.validate_skaffold_config. Read agent.get_task_context before actions governed by user controls.\n'
    + '- Prefer Kaniko through Skaffold cluster builds: skaffold build or skaffold run. If configuration is missing, adapt agent.get_skaffold_example and validate it first. Report failed/unclear infrastructure instead of improvising Kaniko Pods or Jobs; direct invocation requires an explicitly requested, verified workflow.\n'
    + '- Infrastructure uncertainty is a blocker: report it through agent.report_blocked. Do not guess builders, registries, credentials, privileges or public visibility.\n'
    + '- Work in topic branches. Do not push directly to main/master, force-push, or create a remote without user direction.\n';
  const agents = join(cwd, 'AGENTS.md');
  await seed(agents, '# Agent instructions\n');
  if (!(await readFile(agents, 'utf8')).includes(marker)) await appendFile(agents, '\n' + instructions);
  return {specification, agents: 'AGENTS.md', environment: 'skaffold.env'};
}
