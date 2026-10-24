import https from 'node:https';
import {mkdir, readFile, writeFile} from 'node:fs/promises';
import {join} from 'node:path';

const serviceAccountDir = '/var/run/secrets/kubernetes.io/serviceaccount';
export const pushSecret = 'zot-registry-cluster-push-kaniko';

// The single registry host in a dockerconfigjson, or '' when absent or ambiguous.
export function registryHost(dockerConfigJson) {
  let hosts;
  try { hosts = Object.keys(JSON.parse(dockerConfigJson).auths || {}); } catch { return ''; }
  const host = hosts.length === 1 ? hosts[0].replace(/^https?:\/\//, '').replace(/\/+$/, '') : '';
  return /^[a-z0-9.-]+(:\d+)?$/i.test(host) ? host : '';
}

export function inClusterKubeconfig({host, port, namespace, dir = serviceAccountDir}) {
  const server = `https://${host.includes(':') ? `[${host}]` : host}:${port}`;
  return {apiVersion: 'v1', kind: 'Config', 'current-context': 'in-cluster',
    clusters: [{name: 'in-cluster', cluster: {server, 'certificate-authority': `${dir}/ca.crt`}}],
    // tokenFile is reread on each request, so the kubelet's rotation applies.
    users: [{name: 'worker', user: {tokenFile: `${dir}/token`}}],
    contexts: [{name: 'in-cluster', context: {cluster: 'in-cluster', user: 'worker', namespace}}]};
}

function getSecret(namespace, name, dir) {
  return Promise.all([readFile(`${dir}/token`, 'utf8'), readFile(`${dir}/ca.crt`)]).then(([token, ca]) => new Promise((resolve, reject) => {
    const req = https.request({host: process.env.KUBERNETES_SERVICE_HOST, port: process.env.KUBERNETES_SERVICE_PORT || '443',
      path: `/api/v1/namespaces/${encodeURIComponent(namespace)}/secrets/${encodeURIComponent(name)}`,
      ca, timeout: 10000, headers: {authorization: `Bearer ${token.trim()}`}}, res => {
      const chunks = [];
      res.on('data', chunk => chunks.push(chunk));
      res.on('end', () => res.statusCode === 200 ? resolve(JSON.parse(Buffer.concat(chunks))) : reject(new Error(`HTTP ${res.statusCode}`)));
    });
    req.on('timeout', () => req.destroy(new Error('timeout')));
    req.on('error', reject);
    req.end();
  }));
}

// Lets kubectl and Skaffold use the worker's own ServiceAccount. RBAC decides
// which namespaces it may deploy into; the Agent's Pod template sets
// SKAFFOLD_NAMESPACE. Values the Agent's Pod template sets are kept, and all of
// them override a project's skaffold.env. Workers
// without an API token, such as unprivileged support agents, are left alone.
export async function prepareKubeEnv(home, env = process.env, dir = serviceAccountDir) {
  let namespace;
  try { namespace = (await readFile(`${dir}/namespace`, 'utf8')).trim(); await readFile(`${dir}/token`); }
  catch { return; }
  if (!env.KUBECONFIG) {
    const path = join(home, '.kube', 'config');
    await mkdir(join(home, '.kube'), {recursive: true});
    await writeFile(path, JSON.stringify(inClusterKubeconfig({host: env.KUBERNETES_SERVICE_HOST,
      port: env.KUBERNETES_SERVICE_PORT || '443', namespace, dir})), {mode: 0o600});
    env.KUBECONFIG = path;
    env.SKAFFOLD_KUBE_CONTEXT ||= 'in-cluster';
  }
  if (env.SKAFFOLD_DEFAULT_REPO) return;
  // The push Secret names the registry; its credential stays in the Secret.
  try {
    const secret = await getSecret(namespace, pushSecret, dir);
    const host = registryHost(Buffer.from(secret.data?.['.dockerconfigjson'] || '', 'base64').toString('utf8'));
    if (host && env.AGENT_NAME) env.SKAFFOLD_DEFAULT_REPO = `${host}/${env.AGENT_NAME}`;
  } catch { /* no push access: builds stay unconfigured */ }
}
