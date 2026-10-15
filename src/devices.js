import {randomUUID} from 'node:crypto';

const validHostname = /^[a-zA-Z0-9]([a-zA-Z0-9._-]{0,61}[a-zA-Z0-9])?$/;

function sanitizeHostname(raw) {
  const s = String(raw || '').trim().slice(0, 64);
  return validHostname.test(s) ? s : 'device';
}

function deviceInfo(message) {
  const source = message.info && typeof message.info === 'object' ? message.info
    : message.system && typeof message.system === 'object' ? message.system
      : message.systemInfo && typeof message.systemInfo === 'object' ? message.systemInfo : message;
  const fields = /^(?:os|os[_-]?version|platform|arch|architecture|kernel|kernel[_-]?version|distro|distribution|release|version|cpu|cpus|memory)$/i;
  return Object.fromEntries(Object.entries(source).filter(([key, value]) => fields.test(key)
    && (typeof value === 'string' || typeof value === 'number')).slice(0, 20)
    .map(([key, value]) => [key, String(value).slice(0, 100)]));
}

export class DeviceCatalog {
  constructor(kube, config) {
    this.kube = kube;
    this.secretName = config.release + '-devices';
    this.origin = config.origin;
    this._connections = new Map(); // key -> {ws, hostname, connectedAt, pending}
  }

  async readDevices() {
    try {
      const secret = await this.kube.get('secret', this.secretName);
      if (!secret?.data?.['devices.json']) return {};
      return JSON.parse(Buffer.from(secret.data['devices.json'], 'base64').toString('utf8'));
    } catch { return {}; }
  }

  async writeDevices(devices) {
    const object = {apiVersion: 'v1', kind: 'Secret', type: 'Opaque',
      metadata: {name: this.secretName},
      data: {'devices.json': Buffer.from(JSON.stringify(devices)).toString('base64')}};
    await this.kube.ensure(object);
  }

  connectURL(key) {
    return this.origin.replace(/^https?/, match => match === 'https' ? 'wss' : 'ws') + '/devices/' + key;
  }

  handleConnection(key, ws) {
    const conn = {ws, hostname: 'unknown', connectedAt: new Date().toISOString(), pending: new Map()};
    let closed = false;
    ws.on('message', raw => {
      try {
        const msg = JSON.parse(raw);
        if (msg.type === 'hello') {
          conn.hostname = sanitizeHostname(msg.hostname);
          conn.info = deviceInfo(msg);
        } else if (this._connections.get(key) === conn && msg.id && conn.pending.has(msg.id)) {
          const {resolve} = conn.pending.get(msg.id);
          conn.pending.delete(msg.id);
          resolve(msg);
        }
      } catch { /* ignore malformed device messages */ }
    });
    ws.on('close', () => { closed = true; if (this._connections.get(key) === conn) this._connections.delete(key); });
    ws.on('error', () => {
      closed = true;
      if (this._connections.get(key) === conn) this._connections.delete(key);
      try { ws.close(); } catch { /* already closed */ }
    });
    this.readDevices().then(devices => {
      if (!devices[key]) { ws.close(4001, 'Unknown device key'); return; }
      if (closed) return;
      this._connections.set(key, conn);
    }).catch(() => { try { ws.close(4002, 'Configuration error'); } catch { /* ignore */ } });
  }

  async exec(key, cmd, args, timeoutMs = 35000) {
    const conn = this._connections.get(key);
    if (!conn) throw new Error('Device not connected: ' + key);
    const id = randomUUID();
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        conn.pending.delete(id);
        reject(new Error('Device command timed out'));
      }, timeoutMs);
      conn.pending.set(id, {resolve: result => { clearTimeout(timer); resolve(result); }});
      conn.ws.send(JSON.stringify({id, cmd, args}), err => {
        if (err) { clearTimeout(timer); conn.pending.delete(id); reject(err); }
      });
    });
  }

  async list() {
    const devices = await this.readDevices();
    return Object.entries(devices).map(([key, meta]) => {
      const conn = this._connections.get(key);
      return {id: key, label: meta.label || null, createdAt: meta.createdAt,
        hostname: conn?.hostname || null, connectedAt: conn?.connectedAt || null,
        connected: Boolean(conn), info: conn?.info || {}, url: this.connectURL(key)};
    });
  }

  async taskContext(keys = []) {
    const registered = new Map((await this.list()).map(device => [device.id, device]));
    return [...new Set(keys)].map(id => {
      const device = registered.get(id);
      return {id, label: device?.label || null,
        hostname: device?.hostname === 'unknown' ? null : device?.hostname || null,
        connected: Boolean(device?.connected)};
    });
  }

  async create(label) {
    const key = randomUUID().replace(/-/g, '');
    const devices = await this.readDevices();
    devices[key] = {label: label || null, createdAt: new Date().toISOString()};
    await this.writeDevices(devices);
    return {id: key, label: devices[key].label, createdAt: devices[key].createdAt,
      connected: false, hostname: null, connectedAt: null, url: this.connectURL(key)};
  }

  async revoke(key) {
    const devices = await this.readDevices();
    if (!devices[key]) throw new Error('Device not found');
    delete devices[key];
    await this.writeDevices(devices);
    const conn = this._connections.get(key);
    if (conn) { try { conn.ws.close(4003, 'Revoked'); } catch { /* ignore */ } this._connections.delete(key); }
  }

  snapshot() {
    return [...this._connections.entries()].map(([key, conn]) => ({id: key, hostname: conn.hostname, connectedAt: conn.connectedAt}));
  }
}
