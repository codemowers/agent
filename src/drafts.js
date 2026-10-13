import {readdir, readFile} from 'node:fs/promises';
import {join} from 'node:path';
import {Problem, digest, controlsFromMarkdown} from './domain.js';

export class DraftCatalog {
  constructor(directory = process.env.AGENT_DRAFTS_DIR || '/drafts') { this.directory = directory; }
  async get(id) {
    if (typeof id !== 'string' || !/^[a-zA-Z0-9][a-zA-Z0-9._-]{0,120}\.md$/.test(id)) throw new Problem(404, 'Draft not found');
    let content;
    try { content = await readFile(join(this.directory, id), 'utf8'); }
    catch (error) { if (error.code === 'ENOENT') throw new Problem(404, 'Draft not found'); throw error; }
    if (Buffer.byteLength(content) > 128000) throw new Problem(400, 'Draft is too large');
    return {id, title: /^#\s+(.+)$/m.exec(content)?.[1] || id.replace(/\.md$/, ''),
      content, digest: digest(content), size: Buffer.byteLength(content), controls: controlsFromMarkdown(content)};
  }
  async list() {
    let files; try { files = await readdir(this.directory); } catch (error) { if (error.code === 'ENOENT') return []; throw error; }
    return Promise.all(files.filter(file => /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,120}\.md$/.test(file)).sort().map(async file => {
      const draft = await this.get(file); delete draft.content; return draft;
    }));
  }
}
