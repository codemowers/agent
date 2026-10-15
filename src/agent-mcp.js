import {isForgeReference, referenceServer} from './forge.js';
export function agentMcpInitialize() {
  return {protocolVersion: '2025-03-26', capabilities: {tools: {}},
    serverInfo: {name: 'codemowers-agent-namespace', version: '0.1.0'}};
}

export function taskCommunication(task) {
  const issue = task.references?.find(reference => isForgeReference(reference) && reference.kind === 'issue'
    && /^[\w.-]+\/[\w.-]+$/.test(reference.repository || '') && /^[1-9]\d*$/.test(String(reference.id || '')));
  const thread = issue || task.references?.find(reference => isForgeReference(reference) && reference.kind === 'pr'
    && /^[\w.-]+\/[\w.-]+$/.test(reference.repository || '') && /^[1-9]\d*$/.test(String(reference.id || '')));
  return thread ? {primary: {channel: thread.source + (thread.kind === 'issue' ? '_issue' : '_pull_request'),
    url: thread.url || `${referenceServer(thread)}/${thread.repository}/${thread.kind === 'issue' ? 'issues' : thread.source === 'gitea' ? 'pulls' : 'pull'}/${thread.id}`}, secondary: ['email', 'chat']}
    : {primary: {channel: task.origin === 'email' ? 'email' : 'chat'}, secondary: []};
}
