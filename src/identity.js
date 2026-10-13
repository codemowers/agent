import {createHash} from 'node:crypto';

export function agentIdentity(env = process.env) {
  return {harness: 'codemowers/agent', username: env.AGENT_USERNAME || 'agent-smith',
    name: env.AGENT_NAME || env.AGENT_USERNAME || 'Agent Smith',
    email: env.AGENT_EMAIL || 'agent.smith@system.gov'};
}

export function agentActor(executionId, env = process.env) {
  const identity = agentIdentity(env);
  return {type: 'agent', name: identity.name, subject: identity.username, githubUsername: identity.username, executionId,
    ...(identity.email ? {avatarHash: createHash('sha256').update(identity.email.trim().toLowerCase()).digest('hex')} : {})};
}

export function displayIdentity(env = process.env) {
  const {name, subject: username, avatarHash} = agentActor(undefined, env);
  return {name, username, ...(avatarHash ? {avatarHash} : {})};
}
