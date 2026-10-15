import {agentIdentity} from './identity.js';
import {probeRegistry} from './registry.js';
import {checkConnection} from './connectivity.js';
import {agentFetch} from './http.js';

class ConfigurationError extends Error {}

const secretName = 'zot-registry-cluster-push-kaniko';

// Only the verified destination and Secret reference leave the controller.
export class BuildCatalog {
  constructor(kube, env = process.env, request = agentFetch) {
    this.kube = kube; this.request = request;
    this.prefix = env.AGENT_REGISTRY_PREFIX ?? agentIdentity(env).username;
  }
  async configuration() {
    const secret = await this.kube.get('secret', secretName);
    if (!secret) throw new ConfigurationError('Namespace Kaniko push Secret is not provisioned.');
    if (!secret.data?.['config.json']) throw new ConfigurationError('Kaniko push Secret is missing config.json.');
    let config;
    try { config = JSON.parse(Buffer.from(secret.data['config.json'], 'base64').toString('utf8')); }
    catch { throw new ConfigurationError('Namespace push Secret could not be parsed.'); }
    const entries = Object.entries(config.auths || {});
    if (entries.length !== 1) throw new ConfigurationError('Push Secret must identify exactly one registry; destination is ambiguous.');
    const [registry, auth] = entries[0];
    if (!/^[a-zA-Z0-9][a-zA-Z0-9.-]*(?::[0-9]{1,5})?$/.test(registry)) throw new ConfigurationError('Push Secret registry address is invalid.');
    if (!auth || typeof auth !== 'object' || !(typeof auth.auth === 'string' && auth.auth.length
      || typeof auth.username === 'string' && auth.username.length && typeof auth.password === 'string' && auth.password.length
      || typeof auth.identitytoken === 'string' && auth.identitytoken.length)) throw new ConfigurationError('Push Secret has no registry credentials.');
    if (typeof this.prefix !== 'string' || this.prefix.length > 160
      || this.prefix && !/^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:\/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$/.test(this.prefix)) {
      throw new ConfigurationError('AGENT_REGISTRY_PREFIX must be a lowercase repository path or empty.');
    }
    return {registry, auth};
  }
  async list() {
    const {namespace, namespaceIdentity, kubeContext} = this.kube.config;
    try {
      const {registry} = await this.configuration();
      return {status: 'available', namespace, namespaceIdentity, registry, repositoryPrefix: this.prefix,
        pushSecret: {name: secretName, key: 'config.json'}, builder: 'kaniko',
        skaffold: {defaultRepo: registry + (this.prefix ? '/' + this.prefix : ''), kubeContext,
          cluster: {namespace, dockerConfig: {secretName}}},
        detail: 'Provisioned configuration discovered; see Artifact registry startup check for push readiness.'};
    } catch (error) {
      // Only local validation errors are safe to expose, never Kubernetes errors.
      const detail = error instanceof ConfigurationError ? error.message : 'Namespace push Secret could not be read.';
      return {status: 'unavailable', namespace, namespaceIdentity, detail};
    }
  }
  async probe() {
    let result, registry;
    const check = await checkConnection('registry', 'Image push and manifest read-back succeeded', async () => {
      const configuration = await this.configuration();
      registry = configuration.registry;
      const repository = (this.prefix ? this.prefix + '/' : '') + 'agent-connectivity-check/' + this.kube.config.namespace;
      result = await probeRegistry(registry, repository, configuration.auth, this.request);
    }, {timeoutMs: 27000});
    if (result) check.detail += result.cleanup === 'removed'
      ? '; probe image removed' : '; probe image cleanup unavailable' + (result.cleanupStatus ? ` (HTTP ${result.cleanupStatus})` : '');
    return {...check, ...(result ? {probeReference: result.reference} : {}), ...(registry ? {url: `https://${registry}/`} : {})};
  }
}
