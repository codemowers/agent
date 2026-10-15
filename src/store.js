import {isForgeReference, threadKey} from './forge.js';
import {Problem, digest} from './domain.js';

// One task and its conversation are changed atomically, together with the
// request-id receipt. No lost prompt/pause updates across API and reconciliation.
const updateScript = `
local current = redis.call('GET', KEYS[1])
if not current then return -1 end
if redis.call('EXISTS', KEYS[2]) == 1 then return 2 end
if cjson.decode(current).revision ~= tonumber(ARGV[1]) then return 0 end
redis.call('SET', KEYS[1], ARGV[2])
redis.call('SET', KEYS[2], '1')
return 1`;
const createScript = `
local previous = redis.call('GET', KEYS[3])
if (previous or '') ~= ARGV[3] then return false end
if previous and redis.call('EXISTS', KEYS[4]) == 1 then return previous end
redis.call('SET', KEYS[1], ARGV[1])
redis.call('SADD', KEYS[2], ARGV[2])
redis.call('SET', KEYS[3], ARGV[2])
return ARGV[2]`;

export class Store {
  constructor(redis, namespace, release) {
    this.redis = redis;
    this.prefix = `agent:${namespace}:${release}:`;
  }
  key(id) { return this.prefix + 'task:' + id; }
  async get(id) {
    const value = await this.redis.get(this.key(id));
    if (!value) throw new Problem(404, 'Task not found');
    return JSON.parse(value);
  }
  async list() {
    const ids = await this.redis.sMembers(this.prefix + 'tasks');
    if (!ids.length) return [];
    return (await this.redis.mGet(ids.map(id => this.key(id)))).filter(Boolean).map(JSON.parse);
  }
  async create(task, requestId) {
    task.creationRequestHash = digest(requestId);
    const receipt = this.prefix + 'create:' + digest(requestId);
    for (let attempt = 0; attempt < 12; attempt++) {
      const previous = await this.redis.get(receipt);
      // Dragonfly requires every accessed key to be declared. Verify the receipt
      // inside Lua so declaring the previous task key remains safe under races.
      const actual = await this.redis.eval(createScript, {keys: [this.key(task.id), this.prefix + 'tasks',
        receipt, this.key(previous || task.id)], arguments: [JSON.stringify(task), task.id, previous || '']});
      if (actual) return this.get(actual);
    }
    throw new Problem(409, 'Concurrent creation; retry');
  }
  async mutate(id, requestId, fn, expectedRevision) {
    for (let attempt = 0; attempt < 12; attempt++) {
      const task = await this.get(id);
      const receipt = this.prefix + 'receipt:' + digest(id + ':' + requestId);
      if (await this.redis.exists(receipt)) return task;
      if (expectedRevision !== undefined && task.revision !== expectedRevision) throw new Problem(409, 'Task changed; refresh and retry');
      const revision = task.revision;
      fn(task); task.revision++;
      const result = await this.redis.eval(updateScript, {keys: [this.key(id), receipt],
        arguments: [String(revision), JSON.stringify(task)]});
      if (result === 1) return task;
      if (result === 2) return this.get(id);
      if (result === -1) throw new Problem(404, 'Task not found');
    }
    throw new Problem(409, 'Concurrent updates; retry');
  }
  async remove(id) {
    const task = await this.get(id);
    const mappings = (task.references || []).filter(r => isForgeReference(r) && ['issue', 'pr'].includes(r.kind))
      .map(r => this.prefix + 'external:' + digest(threadKey(r)));
    await this.redis.eval(`local t=redis.call('GET',KEYS[1]); if not t then return 0 end;
      local v=cjson.decode(t); if (v.active and v.active~=cjson.null) or not v.deleteRequested then return 0 end;
      redis.call('DEL',KEYS[1]);redis.call('SREM',KEYS[2],ARGV[1]);
      for i=3,#KEYS do if redis.call('GET',KEYS[i])==ARGV[1] then redis.call('DEL',KEYS[i]) end end;return 1`,
    {keys:[this.key(id), this.prefix+'tasks', this.prefix+'create:'+task.creationRequestHash, ...mappings], arguments:[id]});
  }
}
