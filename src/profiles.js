export function githubUsername(actor, identity) {
  if (!actor || actor.type === 'controller') return null;
  const value = actor.type === 'agent' ? identity?.username || actor.githubUsername || actor.subject
    : actor.githubUsername || (actor.provider === 'github' ? actor.name
      : actor.type === 'user' ? actor.subject || actor.name : undefined);
  return typeof value === 'string' && /^[a-z\d](?:[a-z\d-]{0,37}[a-z\d])?$/i.test(value)
    && !/^\d+$/.test(value) && !/^[a-f\d]{8}-[a-f\d]{4}-/i.test(value) ? value : null;
}
