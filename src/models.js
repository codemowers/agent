import {Problem, activity} from './domain.js';
import {agentFetch} from './http.js';

const codexModels = /^(?:gpt-6(?:\.1)?-(?:sol|astra|luna)|gpt-5\.(?:1|2|3|4|5|6)(?:-(?:codex|sol|terra|luna))?)$/;
const claudeModels = /^claude-(?:sonnet|opus|haiku)-\d+(?:-\d+){0,2}$/;

export class ModelCatalog {
  constructor(config, fetchModels = agentFetch) {
    this.config = config;
    this.fetchModels = fetchModels;
    this.discovered = [];
    this.providerModels = {anthropic: [], openai: []};
    this.warnings = [];
  }
  async refresh() {
    const warnings = [];
    const providers = [
      ['anthropic', process.env.ANTHROPIC_API_KEY, 'https://api.anthropic.com/v1/models?limit=100'],
      ['openai', process.env.OPENAI_API_KEY, 'https://api.openai.com/v1/models'],
    ];
    for (const [provider, key, url] of providers) {
      if (!key) {
        this.providerModels[provider] = [];
        warnings.push(`${provider} model discovery unavailable: credential missing`);
        continue;
      }
      try {
        const models = [];
        let next = url;
        for (let page = 0; next && page < 10; page++) {
          const response = await this.fetchModels(next, {headers: provider === 'anthropic'
            ? {'x-api-key': key, 'anthropic-version': '2023-06-01'} : {Authorization: `Bearer ${key}`},
          signal: AbortSignal.timeout(8000), redirect: 'error'});
          if (!response.ok) throw new Error(`HTTP ${response.status}`);
          const body = await response.json();
          if (!Array.isArray(body.data)) throw new Error('Invalid model response');
          for (const item of body.data) {
            if (typeof item.id !== 'string') continue;
            if (provider === 'anthropic' && !claudeModels.test(item.id)) continue;
            if (provider === 'openai' && !codexModels.test(item.id)) continue;
            const backend = provider === 'anthropic' ? 'claude' : 'codex';
            models.push({id: `${provider}:${item.id}`, provider, backend, model: item.id,
              label: `${item.display_name || item.id} · ${provider === 'anthropic' ? 'Anthropic' : 'OpenAI'}`, backends: [backend]});
          }
          next = provider === 'anthropic' && body.has_more && body.last_id
            ? `https://api.anthropic.com/v1/models?limit=100&after_id=${encodeURIComponent(body.last_id)}` : null;
        }
        if (next) throw new Error('Model discovery exceeded page limit');
        this.providerModels[provider] = models;
      } catch (error) { warnings.push(`${provider} model discovery failed: ${error.message}`); }
    }
    this.discovered = [...this.providerModels.anthropic, ...this.providerModels.openai];
    this.warnings = warnings;
    return this.list();
  }
  async list() {
    return {models: [
      {id: 'anthropic:default', provider: 'anthropic', backend: 'claude', model: this.config.model,
        label: this.config.model + ' · Anthropic default', backends: ['claude']},
      {id: 'openai:default', provider: 'openai', backend: 'codex', model: this.config.codexModel || '',
        label: (this.config.codexModel || 'Codex default') + ' · OpenAI default', backends: ['codex']},
      ...this.discovered,
    ], warnings: this.warnings};
  }
  async select(modelId, backend = 'claude') {
    if (!['claude', 'codex'].includes(backend)) throw new Problem(400, 'Unknown worker CLI');
    const {models} = await this.list();
    const model = models.find(m => m.id === (modelId || (backend === 'codex' ? 'openai:default' : 'anthropic:default')));
    if (!model || !model.backends.includes(backend)) throw new Problem(400, 'Model unavailable for this worker CLI');
    return {...model, backend};
  }
}

export function changeModel(task, model, actor) {
  if (task.deleteRequested || ['Completed', 'Cancelled', 'Failed'].includes(task.phase)) throw new Problem(409, 'Task is closed');
  if (task.active) {
    if (task.model?.id === model.id && task.model?.backend === model.backend) {
      if (!task.pendingModel) return;
      task.pendingModel = null;
      activity(task, actor, 'Cancelled the queued model change.', 'webui', {kind: 'model'});
      return;
    }
    task.pendingModel = model;
    activity(task, actor, `Selected ${model.backend} with ${model.label} for the next turn.`, 'webui', {kind: 'model'});
    return;
  }
  if (task.model?.id === model.id && task.model?.backend === model.backend) return;
  task.sessionEpoch = (task.session?.epoch || task.sessionEpoch || 1) + 1;
  task.session = null;
  task.model = model;
  task.pendingModel = null;
  activity(task, actor, `Selected ${model.backend} with ${model.label}. Starting a new conversation with the existing workspace.`,
    'webui', {kind: 'model'});
}
