import {createRemoteJWKSet, customFetch, jwtVerify} from 'jose';
import {githubUsername} from './profiles.js';
import {Problem, digest} from './domain.js';
import {agentFetch} from './http.js';

export function authorized(payload, config, scope) {
  if (!(payload.scope || '').split(' ').includes(scope)) throw new Problem(403, 'Missing API scope');
  const namespaces = payload['codemowers.io/namespaces'] ?? [];
  const groups = payload.groups ?? [];
  const allowed = config.allowedSubjects.includes(payload.sub)
    || config.allowedGroups.some(group => groups.includes(group))
    || (!config.allowedGroups.length && !config.allowedSubjects.length
      && Array.isArray(namespaces) && namespaces.includes(config.namespace));
  if (!allowed) throw new Problem(403, 'No permission for this sandbox');
  const login = githubUsername({type: 'user', githubUsername: payload.preferred_username, subject: payload.sub});
  return {type: 'user', ...(login ? {githubUsername: login} : {}), issuer: payload.iss, subject: payload.sub,
    name: payload.name || payload.email || payload.sub,
    ...(typeof payload.email === 'string' && payload.email_verified === true
      ? {avatarHash: digest(payload.email.trim().toLowerCase())} : {})};
}
export async function authentication(config, {fetchFn = agentFetch, retryDelayMs = 2000} = {}) {
  let lastError;
  for (let attempt = 0; attempt < 3; attempt++) {
    if (attempt > 0) await new Promise(resolve => setTimeout(resolve, attempt * retryDelayMs));
    try {
      const response = await fetchFn(new URL('.well-known/openid-configuration', config.issuer), {signal: AbortSignal.timeout(10000)});
      if (!response.ok) throw new Error('OIDC discovery failed');
      const discovery = await response.json();
      if (discovery.issuer !== config.issuer) throw new Error('OIDC issuer mismatch');
      const jwks = createRemoteJWKSet(new URL(discovery.jwks_uri), {[customFetch]: agentFetch});
      return async (request, scope) => {
        const header = request.headers.authorization;
        if (!header?.startsWith('Bearer ')) throw new Problem(401, 'Sign in to continue');
        let payload;
        try {
          ({payload} = await jwtVerify(header.slice(7), jwks, {
            issuer: config.issuer, audience: config.audience, typ: 'at+jwt',
            algorithms: ['RS256', 'PS256', 'ES256'], requiredClaims: ['sub', 'exp', 'iat'],
          }));
        } catch (error) {
          if (error.code?.startsWith('ERR_J')) throw new Problem(401, 'Invalid or expired access token');
          throw error;
        }
        return authorized(payload, config, scope);
      };
    } catch (error) {
      if (!['TypeError', 'TimeoutError'].includes(error.name)) throw error;
      lastError = error;
    }
  }
  throw lastError;
}
