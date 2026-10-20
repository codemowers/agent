import {marked} from 'marked';
import DOMPurify from 'dompurify';

window.renderAgentMarkdown = content => {
  const body = document.createElement('div');
  body.className = 'markdown';
  body.innerHTML = DOMPurify.sanitize(marked.parse(content || '', {gfm: true}), {
    ALLOWED_TAGS: ['p', 'br', 'hr', 'strong', 'em', 'del', 'blockquote', 'ul', 'ol',
      'li', 'pre', 'code', 'a', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'table',
      'thead', 'tbody', 'tr', 'th', 'td'],
    ALLOWED_ATTR: ['href', 'title', 'start', 'align'],
    ALLOW_DATA_ATTR: false,
    ALLOW_ARIA_ATTR: false,
  });
  for (const link of body.querySelectorAll('a[href]')) {
    if (!['http:', 'https:'].includes(new URL(link.href).protocol)) {
      link.removeAttribute('href');
      continue;
    }
    link.target = '_blank';
    link.rel = 'noopener noreferrer';
  }
  return body;
};
