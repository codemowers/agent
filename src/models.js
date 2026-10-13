import {Problem, activity} from './domain.js';

export class ModelCatalog {
  constructor(config) { this.config = config; }
  async list() {
    return {models: [
      {id: 'anthropic:default', provider: 'anthropic', backend: 'claude', model: this.config.model,
        label: this.config.model + ' · Anthropic', backends: ['claude']},
      {id: 'openai:default', provider: 'openai', backend: 'codex', model: this.config.codexModel || '',
        label: (this.config.codexModel || 'Codex default') + ' · OpenAI', backends: ['codex']},
    ], warnings: []};
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
  if (task.active || task.desiredState !== 'Paused' || ['Completed', 'Cancelled', 'Failed'].includes(task.phase)) {
    throw new Problem(409, 'Pause the task and wait for the worker to stop before changing its CLI or model');
  }
  if (task.model?.id === model.id && task.model?.backend === model.backend) return;
  task.sessionEpoch = (task.session?.epoch || task.sessionEpoch || 1) + 1;
  task.session = null;
  task.model = model;
  activity(task, actor, `Selected ${model.backend} with ${model.label}. Starting a new conversation with the existing workspace.`,
    'webui', {kind: 'model'});
}
