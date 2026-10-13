// Presentation only: retain original events in the task's durable history.
export function groupAgentMessages(events) {
  const groups = [];
  for (const entry of events) {
    const previous = groups.at(-1);
    const response = entry.actor.type === 'agent' && ['progress', 'result', 'tool'].includes(entry.kind);
    const execution = entry.executionId || entry.actor.executionId;
    if (response && execution && previous?.actor.type === 'agent'
      && previous.kind === 'progress' && previous.executionId === execution) {
      if (entry.kind === 'tool') (previous.tools ||= []).push(entry);
      else {
        previous.content += (previous.content ? '\n\n' : '') + entry.content;
        previous.kind = entry.kind;
      }
      if (entry.metrics) previous.metrics = entry.metrics;
    } else groups.push({...entry, executionId: execution,
      ...(entry.kind==='tool'?{kind:'progress',content:'',tools:[entry]}:{})});
  }
  return groups;
}
