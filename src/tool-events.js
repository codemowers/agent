import {safeMcpPayload} from './mcp-payload.js';

export function toolEvents(backend, event, secrets = [], includeMcpPayloads = false, mcpToolIds = new Set()) {
  function command(value) {
    if (typeof value !== 'string') return undefined;
    // Credential operations may contain newly generated or encoded secrets.
    // Keep their existence visible without forwarding their command bodies.
    if (/\b(?:secret|password|token|auth|authorization|login|credential|api[_-]?key)\b|\.docker|https?:\/\/[^\s/]+@|(?:^|\s)(?:-u|--user|--password|--token)(?:\s|=)/i.test(value)) return '[Credential-handling command hidden]';
    let text = value;
    for (const secret of secrets.filter(s=>typeof s==='string'&&s.length>=6)) text=text.split(secret).join('[REDACTED]');
    text=text.replace(/(?:sk-[A-Za-z0-9_-]{12,}|gh[pousr]_[A-Za-z0-9_]{12,}|github_pat_[A-Za-z0-9_]{12,})/g,'[REDACTED]');
    return text.length > 8000 ? text.slice(0,8000)+'\n[Command clipped]' : text;
  }
  function detail(name, input) {
    if (typeof input === 'string') { try { input = JSON.parse(input); } catch { return undefined; } }
    if (!input || typeof input !== 'object') return undefined;
    // Show only routing metadata, never prompts, YAML, log filters or credentials.
    const keys = ['Read', 'Edit', 'Write', 'NotebookEdit'].includes(name) ? ['file_path', 'notebook_path', 'offset', 'limit']
      : ['Glob', 'Grep'].includes(name) ? ['path', 'glob']
      : name.endsWith('__get_engineering_advisory') ? ['path']
      : ['namespace', 'namespaces', 'pod', 'container', 'offering', 'definition', 'name', 'verb', 'group', 'resource', 'subresource', 'limit'];
    const fields = [];
    for (const key of keys) {
      let value = input[key];
      if (Array.isArray(value) && value.length <= 10 && value.every(v => typeof v === 'string')) value = value.join(', ');
      if (typeof value !== 'string' && typeof value !== 'number') continue;
      if (typeof value === 'string' && (!/^[a-zA-Z0-9_./:, *?-]{1,160}$/.test(value)
        || /(?:sk-[A-Za-z0-9_-]{12,}|gh[pousr]_[A-Za-z0-9_]{12,}|github_pat_[A-Za-z0-9_]{12,})/.test(value)
        || secrets.some(secret => secret?.length >= 6 && value.includes(secret)))) continue;
      fields.push(`${key}: ${value}`);
    }
    if (name.endsWith('__get_engineering_advisory') && !input.path) fields.push('path: index');
    return fields.join(' · ').slice(0, 1000) || undefined;
  }
  if (backend === 'codex' && ['item.started','item.completed'].includes(event.type) && event.item?.type === 'mcp_tool_call') {
    const item = event.item, name = `mcp__${item.server}__${item.tool}`;
    return [{kind: 'tool', toolId: item.id, name, detail: detail(name, item.arguments),
      toolState: event.type === 'item.started' ? 'running' : item.error || item.result?.isError || item.status === 'failed' ? 'failed' : 'completed',
      ...(includeMcpPayloads ? {[event.type === 'item.started' ? 'mcpRequest' : 'mcpResponse']:
        safeMcpPayload(event.type === 'item.started' ? item.arguments : item.error || item.result, secrets)} : {})}];
  }
  if (backend === 'codex' && ['item.started','item.completed'].includes(event.type) && event.item?.type === 'command_execution') {
    return [{kind:'tool',toolId:event.item.id,name:'Shell',command:command(event.item.command),
      toolState:event.type==='item.started'?'running':event.item.exit_code || event.item.status==='failed' ? 'failed':'completed',
      ...(Number.isInteger(event.item.exit_code)?{exitCode:event.item.exit_code}:{})}];
  }
  if (backend === 'claude' && event.type === 'assistant') {
    return (event.message?.content||[]).filter(block=>block.type==='tool_use').map(block=>({
      kind:'tool',toolId:block.id,name:block.name,toolState:'running',
      detail: detail(block.name, block.input),
      ...(includeMcpPayloads && block.name?.startsWith('mcp__') ? {mcpRequest: safeMcpPayload(block.input, secrets)} : {}),
      ...(block.name==='Bash'?{command:command(block.input?.command)}:{})}));
  }
  if (backend === 'claude' && event.type === 'user') {
    return (event.message?.content||[]).filter(block=>block.type==='tool_result').map(block=>({
      kind:'tool',toolId:block.tool_use_id,toolState:block.is_error?'failed':'completed',
      ...(includeMcpPayloads && mcpToolIds.has(block.tool_use_id) ? {mcpResponse: safeMcpPayload(block.content, secrets)} : {})}));
  }
  return [];
}
