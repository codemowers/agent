import {readFile} from 'node:fs/promises';
import Ajv from 'ajv';
import {parseAllDocuments} from 'yaml';
import {Problem} from './domain.js';

export class SkaffoldSchema {
  constructor(directory = new URL('../schemas/', import.meta.url)) { this.directory = directory; }
  async load() {
    this.loaded ||= Promise.all([
      readFile(new URL('skaffold-v4beta11.json', this.directory), 'utf8'),
      readFile(new URL('skaffold-version.txt', this.directory), 'utf8'),
    ]).then(([raw, version]) => {
      const schema = JSON.parse(raw);
      const validator = new Ajv({strict: false, allErrors: true, validateFormats: false}).compile(schema);
      return {schema, validator, version: version.trim(), apiVersion: 'skaffold/v4beta11'};
    });
    return this.loaded;
  }
  async get(definition = 'SkaffoldConfig') {
    const {schema, version, apiVersion} = await this.load();
    if (typeof definition !== 'string' || !Object.hasOwn(schema.definitions, definition)) {
      throw new Problem(400, 'Unknown Skaffold schema definition');
    }
    const fragment = schema.definitions[definition], references = new Set();
    const scan = value => {
      if (!value || typeof value !== 'object') return;
      if (typeof value.$ref === 'string' && value.$ref.startsWith('#/definitions/')) references.add(value.$ref.slice(14));
      for (const child of Object.values(value)) scan(child);
    };
    scan(fragment);
    return {version, apiVersion, definition, schema: fragment, referencedDefinitions: [...references].sort(),
      guidance: 'This is a schema fragment from the installed runner CLI. Retrieve referenced definitions with this tool; use validate_skaffold_config for complete offline YAML validation. Manifests belong in manifests.rawYaml, not deploy.kubectl. This validates syntax only, not RBAC, admission, build or deployment success.'};
  }
  async validate(content) {
    if (typeof content !== 'string' || !content.trim() || content.length > 64000) {
      throw new Problem(400, 'Expected Skaffold YAML, at most 64000 characters');
    }
    const {validator, version, apiVersion} = await this.load();
    const documents = parseAllDocuments(content, {uniqueKeys: true});
    if (!documents.length || documents.length > 10 || documents.some(doc => doc.errors.length || doc.warnings.length)) {
      return {valid: false, version, apiVersion, errors: [{message: 'Invalid YAML, duplicate keys, unsupported tags, or too many documents.'}]};
    }
    const errors = [];
    for (const [index, document] of documents.entries()) {
      let value;
      try { value = document.toJS({maxAliasCount: 0}); }
      catch { errors.push({document: index + 1, message: 'YAML aliases are not supported.'}); continue; }
      if (!validator(value)) {
        const failures = validator.errors || [];
        const specific = failures.filter(error => error.keyword === 'additionalProperties');
        for (const error of (specific.length ? specific : failures).slice(0, 12)) errors.push({
          document: index + 1, path: error.instancePath || '/', message: error.message,
          ...(error.keyword === 'additionalProperties' ? {unknownField: error.params.additionalProperty} : {}),
        });
      }
    }
    return {valid: !errors.length, version, apiVersion, errors,
      scope: 'Offline YAML/schema validation only; no commands executed or Kubernetes resources changed.'};
  }
  async example(context, image = 'app') {
    if (!/^[a-z0-9][a-z0-9._-]{0,62}$/.test(image)) throw new Problem(400, 'Expected a simple image artifact name');
    if (context.status !== 'available') return {status: 'unavailable', detail: context.detail};
    const config = {apiVersion: 'skaffold/v4beta11', kind: 'Config', metadata: {name: image},
      build: {cluster: {...context.skaffold.cluster}, artifacts: [{image, context: '.', kaniko: {dockerfile: 'Dockerfile'}}]},
      manifests: {rawYaml: ['k8s/*.yaml']}, deploy: {kubectl: {}}};
    const {version} = await this.load();
    return {version, config, defaultRepo: context.skaffold.defaultRepo,
      command: 'skaffold run',
      environment: {SKAFFOLD_DEFAULT_REPO: context.skaffold.defaultRepo, SKAFFOLD_NAMESPACE: context.namespace,
        SKAFFOLD_KUBE_CONTEXT: context.skaffold.kubeContext},
      note: 'Use the existing mounted kubeconfig. Check permissions through Driftmower before running, including get on the named push Secret. A denied permission is a blocker, not permission to bypass Skaffold.'};
  }
}
