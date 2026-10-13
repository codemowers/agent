export function agentMcpInitialize() {
  return {protocolVersion: '2025-03-26', capabilities: {tools: {}},
    serverInfo: {name: 'codemowers-agent-namespace', version: '0.1.0'}};
}

export function taskCommunication(task) {
  const issue = task.references?.find(reference => reference.source === 'github' && reference.kind === 'issue'
    && /^[\w.-]+\/[\w.-]+$/.test(reference.repository || '') && /^[1-9]\d*$/.test(String(reference.id || '')));
  return issue ? {primary: {channel: 'github_issue', url: `https://github.com/${issue.repository}/issues/${issue.id}`},
    secondary: ['email', 'chat']}
    : {primary: {channel: task.origin === 'email' ? 'email' : 'chat'}, secondary: []};
}
