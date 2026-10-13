import {marked} from 'marked';
import DOMPurify from 'dompurify';

export function markdown(content) {
  const body = document.createElement('div');
  body.className = 'markdown';
  body.innerHTML = DOMPurify.sanitize(marked.parse(content || '', {gfm: true}), {
    ALLOWED_TAGS: ['p', 'br', 'hr', 'strong', 'em', 'del', 'blockquote', 'ul', 'ol',
      'li', 'pre', 'code', 'a', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'table',
      'thead', 'tbody', 'tr', 'th', 'td'],
    ALLOWED_ATTR: ['href', 'title', 'start', 'align'],
    ALLOW_DATA_ATTR: false,
    ALLOW_ARIA_ATTR: false
  });
  for (const code of body.querySelectorAll('code')) {
    if (code.closest('pre') || !/^https?:\/\/[^\s<>"`]+$/i.test(code.textContent)) continue;
    try {
      const url = new URL(code.textContent);
      if (!['http:', 'https:'].includes(url.protocol)) continue;
      const link = document.createElement('a'); link.href = url.href; link.textContent = code.textContent;
      code.replaceChildren(link);
    } catch { /* Keep malformed URLs as code. */ }
  }
  for (const link of body.querySelectorAll('a[href]')) {
    link.target = '_blank'; link.rel = 'noopener noreferrer';
  }
  return body;
}
