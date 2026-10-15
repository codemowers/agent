import {forgeConfiguration} from './forge.js';
import {spawn} from 'node:child_process';
import {workerIdleTimeoutSeconds} from './domain.js';
import {driftmowerTokenProjection} from './mcp-auth.js';
import {agentIdentity} from './identity.js';

export function kubectlToolImage(gitVersion) {
  const version = /^v?(\d+\.\d+\.\d+)(?:[-+].*)?$/.exec(gitVersion || '')?.[1];
  if (!version) throw new Error('Kubernetes API did not report a valid server version');
  return {name: 'kubectl', image: 'registry.k8s.io/kubectl:v' + version, binPath: 'bin'};
}

export function command(bin, args, input, options = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn(bin, args, {stdio: ['pipe', 'pipe', 'pipe'], ...options});
    let out = '', err = '';
    const timer = setTimeout(() => child.kill('SIGKILL'), 30000);
    child.stdout.on('data', chunk => { out += chunk; if (out.length > 4000000) child.kill('SIGKILL'); });
    child.stderr.on('data', chunk => { err = (err + chunk).slice(-8000); });
    child.on('error', reject);
    child.on('close', code => {
      clearTimeout(timer);
      if (code) { const error = new Error(`${bin} exited ${code}: ${err}`); error.exitCode = code; reject(error); }
      else resolve(out);
    });
    child.stdin.end(input);
  });
}
export class Kube {
  constructor(config) { this.config = config; }
  async run(args, input) {
    return command('kubectl', ['--request-timeout=20s', '-n', this.config.namespace, ...args], input);
  }
  async get(kind, name) {
    const out = await this.run(['get', kind, name, '--ignore-not-found', '-o', 'json']);
    return out ? JSON.parse(out) : null;
  }
  async create(object) { return this.run(['create', '-f', '-', '-o', 'json'], JSON.stringify(object)); }
  async ensure(object) {
    // Server-side apply makes partial Pod/PVC creation recoverable by name.
    return this.run(['apply', '--server-side', '--field-manager=codemowers-agent', '-f', '-'], JSON.stringify(object));
  }
  async leader() {
    const name = this.config.release + '-controller';
    const lease = await this.get('lease', name);
    const now = new Date();
    if (lease && lease.spec.holderIdentity !== this.config.instance
      && Date.parse(lease.spec.renewTime) + lease.spec.leaseDurationSeconds * 1000 > now.getTime()) return false;
    const object = {apiVersion: 'coordination.k8s.io/v1', kind: 'Lease', metadata: {name},
      spec: {holderIdentity: this.config.instance, leaseDurationSeconds: 60, renewTime: now.toISOString().replace(/Z$/, '000Z')}};
    if (lease) {
      object.metadata.resourceVersion = lease.metadata.resourceVersion;
      await this.run(['replace', '-f', '-'], JSON.stringify(object));
    } else await this.create(object);
    return true;
  }
  labels(task, execution) {
    return {'app.kubernetes.io/instance': this.config.release, 'agents.codemowers.io/managed': 'true',
      'agents.codemowers.io/task-id': task.id, ...(execution ? {'agents.codemowers.io/execution-id': execution.id} : {})};
  }
  pvc(task) {
    return {apiVersion: 'v1', kind: 'PersistentVolumeClaim', metadata: {
      name: `${this.config.release}-task-${task.id}`, labels: this.labels(task)},
    spec: {accessModes: ['ReadWriteOnce'], storageClassName: this.config.storageClass,
      resources: {requests: {storage: this.config.workspaceSize}}}};
  }
  async workerPod(task, infrastructure) {
    const {serverVersion} = JSON.parse(await this.run(['version', '-o', 'json']));
    const tools = [kubectlToolImage(serverVersion?.gitVersion),
      ...(this.config.toolImages || []).filter(tool => tool.name !== 'kubectl')];
    return this.pod(task, infrastructure, tools);
  }
  pod(task, infrastructure, toolImages = this.config.toolImages || []) {
    const cfg = this.config, execution = task.active;
    const identity = cfg.identity || agentIdentity({}), forge = cfg.forge || forgeConfiguration({});
    const toolMounts = toolImages.map(tool => ({name: 'tool-' + tool.name,
      mountPath: tool.mountPath || '/opt/tools/' + tool.name + '/bin', subPath: tool.binPath, readOnly: true}));
    const toolVolumes = toolImages.map(tool => ({name: 'tool-' + tool.name, image: {reference: tool.image, pullPolicy: 'IfNotPresent'}}));
    const model = execution.model || task.model;
    const backend = model?.backend || 'claude';
    if (model && model.provider !== (backend === 'codex' ? 'openai' : 'anthropic')) {
      throw new Error('Unsupported model provider for worker CLI');
    }
    const secrets = [
      backend === 'codex'
        ? {name: 'OPENAI_API_KEY', valueFrom: {secretKeyRef: {name: cfg.openaiSecret, key: 'token', optional: true}}}
        : {name: 'ANTHROPIC_API_KEY', valueFrom: {secretKeyRef: {name: cfg.anthropicSecret, key: 'token', optional: true}}},
      {name: 'GH_TOKEN', valueFrom: {secretKeyRef: {name: cfg.githubSecret, key: 'token', optional: true}}},
    ];
    const env = {
      HOME: '/workspace/home', AGENT_MODE: 'worker', AGENT_TASK_ID: task.id,
      AGENT_EXECUTION_ID: execution.id, AGENT_REPORT_TOKEN: execution.token,
      AGENT_SERVICE_URL: cfg.internalURL, AGENT_STATE_DIR: '/workspace',
      AGENT_MODEL: model ? model.model : cfg.model, AGENT_MCP_CONFIG_FILE: '/mcp/mcp.json',
      AGENT_USERNAME: identity.username, AGENT_NAME: identity.name, AGENT_EMAIL: identity.email,
      KUBECONFIG: '/config/kubeconfig',
      PATH: [...toolImages.map(tool => tool.mountPath ? tool.mountPath + '/bin' : '/opt/tools/' + tool.name + '/bin'),
        '/usr/local/sbin', '/usr/local/bin', '/usr/sbin', '/usr/bin', '/sbin', '/bin'].join(':'),
      ...(toolImages.some(tool => tool.name === 'go') ? {
        GOROOT: toolImages.find(tool => tool.name === 'go').mountPath || '/opt/tools/go',
        GOPATH: '/workspace/home/go', GOCACHE: '/workspace/home/.cache/go-build', GOTOOLCHAIN: 'local',
      } : {}),
      SKAFFOLD_NAMESPACE: cfg.namespace,
      ...(cfg.kubeContext ? {SKAFFOLD_KUBE_CONTEXT: cfg.kubeContext, AGENT_KUBE_CONTEXT: cfg.kubeContext} : {}),
      ...(infrastructure?.status === 'available' ? {SKAFFOLD_DEFAULT_REPO: infrastructure.skaffold.defaultRepo} : {}),
      AGENT_BACKEND: backend,
      AGENT_MCP_PAYLOADS_ENABLED: String(cfg.mcpPayloadsEnabled === true),
      AGENT_NAMESPACE: cfg.namespace, GH_HOST: new URL(forge.serverURL).host,
      AGENT_FORGE_PROVIDER: forge.provider, AGENT_FORGE_URL: forge.serverURL, AGENT_FORGE_API_URL: forge.apiURL, GIT_TERMINAL_PROMPT: '0',
      AGENT_INSTRUCTIONS_FILE: '/config/instructions.md', AGENT_BUDGET_USD: String(cfg.budget),
      AGENT_ENVELOPE_FILE: '/execution/envelope.json',
      AGENT_IDLE_TIMEOUT_SECONDS: String(execution.idleTimeoutSeconds ?? workerIdleTimeoutSeconds(task)),
    };
    return {apiVersion: 'v1', kind: 'Pod', metadata: {name: execution.podName, labels: this.labels(task, execution),
      finalizers: ['agents.codemowers.io/result-recorded']},
      spec: {hostUsers: false, serviceAccountName: cfg.release + '-worker',
        restartPolicy: 'Never', terminationGracePeriodSeconds: 45,
        securityContext: {runAsNonRoot: true, runAsUser: 65534, runAsGroup: 65534, fsGroup: 65534},
        containers: [{name: 'agent', image: cfg.image, command: ['node', '/app/src/main.js'],
          env: [...Object.entries(env).map(([name, value]) => ({name, value})), ...secrets],
          securityContext: {readOnlyRootFilesystem: true, allowPrivilegeEscalation: false,
            capabilities: {drop: ['ALL']}},
          volumeMounts: [{name: 'workspace', mountPath: '/workspace'}, {name: 'tmp', mountPath: '/tmp'},
            ...toolMounts,
            {name: 'config', mountPath: '/config', readOnly: true},
            {name: 'mcp-token', mountPath: '/var/run/agent-mcp', readOnly: true},
            {name: 'mcp-config', mountPath: '/mcp', readOnly: true},
            {name: 'execution', mountPath: '/execution', readOnly: true}]}],
        volumes: [{name: 'workspace', persistentVolumeClaim: {claimName: `${cfg.release}-task-${task.id}`}},
          ...toolVolumes,
          {name: 'tmp', emptyDir: {sizeLimit: '4Gi'}},
          {name: 'config', configMap: {name: cfg.release + '-config'}},
          {name: 'mcp-token', projected: {sources: [{serviceAccountToken: {...driftmowerTokenProjection}}]}},
          {name: 'mcp-config', configMap: {name: cfg.release + '-mcp', optional: true}},
          {name: 'execution', configMap: {name: execution.podName + '-input'}}]}};
  }
  envelope(task) {
    return {apiVersion: 'v1', kind: 'ConfigMap', metadata: {name: task.active.podName + '-input', labels: this.labels(task, task.active)},
      immutable: true, data: {'envelope.json': JSON.stringify({id: task.id, objective: task.objective,
        session: task.session, sessionId: task.active.sessionId, repository: task.repository, references: task.references, origin: task.origin,
        previousSummary: task.lastExecution?.result?.summary,
        prompts: task.events.filter(e => task.active.prompts.includes(e.id))}),
        ...(task.draft ? {'draft.md': task.draft.content} : {})}};
  }
}
