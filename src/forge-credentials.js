import {mkdir, rm, symlink, writeFile} from 'node:fs/promises';
import {join} from 'node:path';

// Environment variables gh and tea read a token or an environment login
// from. Each applies to every host of its kind, so with several hosts of a
// kind the worker hands the CLI generated per-host configuration instead.
const ghDotComEnv = ['GH_TOKEN', 'GITHUB_TOKEN'];
const ghEnterpriseEnv = ['GH_ENTERPRISE_TOKEN', 'GITHUB_ENTERPRISE_TOKEN'];
const teaEnv = ['GITEA_INSTANCE_URL'];

export function gitProviders(env = process.env) {
  let providers;
  try { providers = JSON.parse(env.AGENT_GIT_PROVIDERS || '[]'); } catch { return []; }
  return Array.isArray(providers) ? providers.filter(provider => provider && ['github', 'gitea'].includes(provider.type) &&
    typeof provider.serverURL === 'string' && /^https:\/\/[^/?#@]+(\/[^?#]*)?$/.test(provider.serverURL) &&
    typeof provider.tokenEnv === 'string' && /^[A-Za-z_][A-Za-z0-9_]*$/.test(provider.tokenEnv)) : [];
}

function dotCom(provider) {
  const host = new URL(provider.serverURL).hostname.toLowerCase();
  return host === 'github.com' || host.endsWith('.ghe.com');
}

// ambiguousEnv names the variables to drop from the CLI environment: a token
// variable gh or tea would send to every host of its kind while several
// hosts of that kind are configured.
export function ambiguousEnv(providers) {
  const github = providers.filter(provider => provider.type === 'github');
  const drop = [];
  if (github.filter(dotCom).length > 1) drop.push(...ghDotComEnv);
  if (github.filter(provider => !dotCom(provider)).length > 1) drop.push(...ghEnterpriseEnv);
  if (providers.filter(provider => provider.type === 'gitea').length > 1) drop.push(...teaEnv);
  return drop;
}

function quote(value) {
  return JSON.stringify(value);
}

// ghHosts renders gh's hosts.yml with one login per GitHub host.
export function ghHosts(providers, env) {
  return providers.filter(provider => provider.type === 'github' && env[provider.tokenEnv]).map(provider => {
    const host = new URL(provider.serverURL).host;
    const token = quote(env[provider.tokenEnv]);
    return `${quote(host)}:\n  user: x-access-token\n  oauth_token: ${token}\n  git_protocol: https\n` +
      `  users:\n    x-access-token:\n      oauth_token: ${token}\n`;
  }).join('');
}

// teaConfig renders tea's config.yml with one login per Gitea instance; tea
// picks the login whose URL matches the repository's remote.
export function teaConfig(providers, env) {
  const logins = providers.filter(provider => provider.type === 'gitea' && env[provider.tokenEnv]).map((provider, index) =>
    `  - name: ${quote(new URL(provider.serverURL).host + new URL(provider.serverURL).pathname.replace(/\/$/, ''))}\n` +
    `    url: ${quote(provider.serverURL)}\n    token: ${quote(env[provider.tokenEnv])}\n` +
    `    default: ${index === 0}\n    version_check: false\n`);
  return logins.length ? `logins:\n${logins.join('')}preferences:\n  editor: false\n` : '';
}

// gitCredentialConfig returns git config entries that answer credential
// requests for each provider from its token file in the memory volume, so no
// token is written to the workspace and dropping ambiguous variables from
// the CLI environment leaves Git working.
export function gitCredentialConfig(providers, giteaUser, dir) {
  return providers.flatMap((provider, index) => {
    const scope = `credential.${provider.serverURL.replace(/\/$/, '')}`;
    const user = provider.type === 'gitea' ? giteaUser : 'x-access-token';
    const file = join(dir, 'git', String(index));
    return [[`${scope}.username`, user],
      [`${scope}.helper`, `!f() { test "$1" = get && printf 'password=%s\\n' "$(cat ${file})"; }; f`]];
  });
}

// prepareForgeCredentials writes the gh and tea configuration into dir, a
// memory-backed volume, and Git credential helpers into the global Git
// config. It returns the variables to remove from the CLI environment.
export async function prepareForgeCredentials({env = process.env, dir, home, giteaUser, run}) {
  const providers = gitProviders(env);
  if (!providers.length || !dir) return [];
  await mkdir(join(dir, 'git'), {recursive: true, mode: 0o700});
  for (const [index, provider] of providers.entries()) {
    await writeFile(join(dir, 'git', String(index)), env[provider.tokenEnv] || '', {mode: 0o600});
  }
  for (const [key, value] of gitCredentialConfig(providers, giteaUser, dir)) {
    await run('git', ['config', '--global', '--replace-all', key, value]);
  }
  {
    const hosts = ghHosts(providers, env);
    if (hosts) {
      await mkdir(join(dir, 'gh'), {recursive: true, mode: 0o700});
      await writeFile(join(dir, 'gh', 'hosts.yml'), hosts, {mode: 0o600});
      env.GH_CONFIG_DIR = join(dir, 'gh');
    }
    const tea = teaConfig(providers, env);
    if (tea) {
      await mkdir(join(dir, 'tea'), {recursive: true, mode: 0o700});
      await writeFile(join(dir, 'tea', 'config.yml'), tea, {mode: 0o600});
      // tea reads only its XDG path, which lives in the workspace home; the
      // link keeps the tokens in memory.
      const config = join(env.XDG_CONFIG_HOME || join(home, '.config'));
      await mkdir(config, {recursive: true});
      await rm(join(config, 'tea'), {recursive: true, force: true});
      await symlink(join(dir, 'tea'), join(config, 'tea'));
    }
  }
  return ambiguousEnv(providers);
}
