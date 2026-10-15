export function formattedJson(value) {
  const pre = document.createElement('pre');
  pre.className = 'mcp-json';
  const json = JSON.stringify(value, null, 2) ?? 'null';
  const token = /"(?:\\.|[^"\\])*"(?=\s*:)|"(?:\\.|[^"\\])*"|\b(?:true|false|null|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)\b/g;
  let offset = 0;
  for (const match of json.matchAll(token)) {
    pre.append(document.createTextNode(json.slice(offset, match.index)));
    const span = document.createElement('span');
    span.className = match[0].startsWith('"') ? (/^\s*:/.test(json.slice(match.index + match[0].length)) ? 'json-key' : 'json-string') : 'json-value';
    span.textContent = match[0];
    pre.append(span);
    offset = match.index + match[0].length;
  }
  pre.append(document.createTextNode(json.slice(offset)));
  return pre;
}
