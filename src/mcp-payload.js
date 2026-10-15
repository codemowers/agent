const sensitiveKey = /(?:password|secret|token|authorization|credential|api[_-]?key|private[_-]?key|cookie|configjson)/i;
const credential = /(?:sk-[A-Za-z0-9_*.\-]{12,}|gh[pousr]_[A-Za-z0-9_]{12,}|github_pat_[A-Za-z0-9_]{12,}|Bearer\s+[^\s"']+|Basic\s+[A-Za-z0-9+/=]+)/gi;

export function safeMcpPayload(value, secrets = []) {
  const seen = new WeakSet();
  function clean(input, depth = 0) {
    if (input === null || typeof input === 'boolean' || typeof input === 'number') return input;
    if (typeof input === 'string') {
      if (/^\s*[\[{]/.test(input) && depth < 11) {
        try { return clean(JSON.parse(input), depth + 1); } catch {}
      }
      if (/(?:secret|password|token|authorization|credential|api[_-]?key)\s*[=:]/i.test(input)) return '[Credential-bearing text hidden]';
      let output = input.slice(0, 16000).replace(credential, '[REDACTED]');
      for (const secret of secrets.filter(item => typeof item === 'string' && item.length >= 6)) output = output.split(secret).join('[REDACTED]');
      return output + (input.length > 16000 ? '\n[Clipped]' : '');
    }
    if (typeof input !== 'object' || depth >= 12 || seen.has(input)) return '[Omitted]';
    seen.add(input);
    if (Array.isArray(input)) return input.slice(0, 100).map(item => clean(item, depth + 1));
    return Object.fromEntries(Object.entries(input).slice(0, 100).map(([key, item]) =>
      [key, sensitiveKey.test(key) ? '[REDACTED]' : clean(item, depth + 1)]));
  }
  const output = clean(value);
  return Buffer.byteLength(JSON.stringify(output)) <= 32000 ? output : '[Payload exceeds 32 KB]';
}

export function mcpPayloadEnabled(value = process.env.AGENT_MCP_PAYLOADS_ENABLED) {
  return value === 'true';
}
