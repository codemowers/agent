import {sharedTask} from './sharing.js';
import {digest} from './domain.js';
import {transcriptText} from './refinement-defaults.js';

export function transcriptDraft(source) {
  const task = sharedTask(source);
  const content = transcriptText({...task, objective: source.objective});
  return {id: 'transcript-' + task.id, title: 'Refine: ' + task.title, digest: digest(content), content, controls: {}};
}
