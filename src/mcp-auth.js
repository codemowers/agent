import {readFile} from 'node:fs/promises';
import http from 'node:http';
import {Readable} from 'node:stream';
import {pipeline} from 'node:stream/promises';
import {agentFetch} from './http.js';

export async function mcpRequest(server, options = {}, request = agentFetch) {
  const headers = {...server.headers, ...options.headers};
  if (server.tokenFile) {
    for (const key of Object.keys(headers)) if (key.toLowerCase() === 'authorization') delete headers[key];
    headers.Authorization = 'Bearer ' + (await readFile(server.tokenFile, 'utf8')).trim();
  }
  return request(server.url, {...options, headers, redirect: 'error'});
}

// CLI config stays credential-free. Each forwarded request reads kubelet's
// rotating audience-bound token rather than pinning a token for a whole turn.
export async function workerMcpConfig(config) {
  const mcpServers = {}, bridges = [];
  try {
    for (const [name, definition] of Object.entries(config.mcpServers || {})) {
      const remote = definition;
      if (!remote.tokenFile) { mcpServers[name] = remote; continue; }
      const server = http.createServer(async (req, res) => {
        try {
          if (req.url !== '/mcp' || !['GET', 'POST', 'DELETE'].includes(req.method)) { res.writeHead(404); res.end(); return; }
          const chunks = []; let size = 0;
          for await (const chunk of req) { size += chunk.length; if (size > 1024 * 1024) { res.writeHead(413); res.end(); return; } chunks.push(chunk); }
          const headers = {};
          for (const key of ['accept', 'content-type', 'mcp-session-id', 'mcp-protocol-version', 'last-event-id']) {
            if (req.headers[key]) headers[key] = req.headers[key];
          }
          const abort = new AbortController();
          res.on('close', () => { if (!res.writableEnded) abort.abort(); });
          const response = await mcpRequest(remote, {method: req.method, headers, signal: abort.signal,
            ...(req.method === 'POST' ? {body: Buffer.concat(chunks)} : {})});
          const outputHeaders = {};
          for (const key of ['content-type', 'mcp-session-id', 'www-authenticate']) {
            if (response.headers.has(key)) outputHeaders[key] = response.headers.get(key);
          }
          res.writeHead(response.status, outputHeaders);
          if (response.body) await pipeline(Readable.fromWeb(response.body), res); else res.end();
        } catch { if (!res.headersSent) res.writeHead(502); res.end('MCP connection unavailable'); }
      });
      await new Promise((resolve, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', resolve); });
      server.unref(); bridges.push(server);
      mcpServers[name] = {type: 'http', url: `http://127.0.0.1:${server.address().port}/mcp`};
    }
    return {mcpServers, close() { for (const server of bridges) { server.closeAllConnections(); server.close(); } }};
  } catch (error) { for (const server of bridges) server.close(); throw error; }
}
