import {createHash, randomUUID} from 'node:crypto';
import {agentFetch} from './http.js';

const digest = bytes => 'sha256:' + createHash('sha256').update(bytes).digest('hex');
const manifestType = 'application/vnd.oci.image.manifest.v1+json';

// A real push/read-back check against only the provisioned HTTPS registry.
// Each check owns a unique zero-layer image; cleanup never touches application tags.
export async function probeRegistry(registry, repository, auth, request = agentFetch) {
  const origin = new URL(`https://${registry}/`);
  const authorization = auth.identitytoken ? `Bearer ${auth.identitytoken}`
    : `Basic ${auth.auth || Buffer.from(`${auth.username}:${auth.password}`).toString('base64')}`;
  const signal = AbortSignal.timeout(20000);
  const root = `/v2/${repository}/`;
  const tag = 'check-' + randomUUID();
  const config = Buffer.from(JSON.stringify({architecture: 'amd64', os: 'linux',
    rootfs: {type: 'layers', diff_ids: []}, config: {}, created: new Date().toISOString(),
    annotations: {'io.codemowers.agent.connectivity': tag}}));
  const manifest = Buffer.from(JSON.stringify({schemaVersion: 2, mediaType: manifestType,
    config: {mediaType: 'application/vnd.oci.image.config.v1+json', digest: digest(config), size: config.length}, layers: []}));
  const manifestDigest = digest(manifest);
  let upload, published = false, verified = false, cleanup = 'not-needed', cleanupStatus;
  async function call(path, method, expected, body, type, abort = signal) {
    const url = new URL(path, origin);
    if (url.origin !== origin.origin || url.username || url.password || !url.pathname.startsWith(root)) {
      throw new Error('Registry upload destination rejected');
    }
    const response = await request(url, {method, redirect: 'error', signal: abort,
      headers: {Authorization: authorization, ...(type ? {'Content-Type': type} : {}), Accept: manifestType}, body});
    if (!expected.includes(response.status)) {
      await response.body?.cancel();
      throw Object.assign(new Error('Registry request rejected'), {upstreamStatus: response.status});
    }
    return response;
  }
  try {
    const started = await call(root + 'blobs/uploads/', 'POST', [202]);
    const location = started.headers.get('Location');
    await started.body?.cancel();
    if (!location) throw new Error('Registry did not provide an upload location');
    upload = new URL(location, origin);
    if (!upload.pathname.startsWith(root + 'blobs/uploads/')) throw new Error('Registry upload path rejected');
    upload.searchParams.set('digest', digest(config));
    const uploaded = await call(upload, 'PUT', [201], config, 'application/octet-stream');
    await uploaded.body?.cancel();
    upload = null;
    const pushed = await call(root + 'manifests/' + tag, 'PUT', [201], manifest, manifestType);
    published = true;
    await pushed.body?.cancel();
    const readback = await call(root + 'manifests/' + tag, 'GET', [200]);
    // Bound the response even if a misconfigured endpoint streams arbitrary output.
    const reader = readback.body.getReader(), chunks = [];
    let size = 0;
    try {
      for (;;) {
        const {value, done} = await reader.read();
        if (done) break;
        size += value.length;
        if (size > 8192) throw new Error('Registry manifest response too large');
        chunks.push(value);
      }
    } finally { await reader.cancel(); }
    if (digest(Buffer.concat(chunks)) !== manifestDigest) throw new Error('Registry manifest read-back mismatch');
    verified = true;
  } finally {
    if (published || upload) {
      try {
        const removed = await call(published ? root + 'manifests/' + manifestDigest : upload,
          'DELETE', [202, 204, 404], undefined, undefined, AbortSignal.timeout(5000));
        await removed.body?.cancel();
        cleanup = 'removed';
      } catch (error) { cleanup = 'unavailable'; cleanupStatus = error.upstreamStatus; }
    }
  }
  return {verified, cleanup, ...(Number.isInteger(cleanupStatus) ? {cleanupStatus} : {}), reference: `${registry}/${repository}:${tag}`};
}
