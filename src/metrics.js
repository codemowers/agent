import http from 'node:http';

// A minimal Prometheus registry for the worker. Prometheus adds namespace,
// pod, agent and task from the PodMonitor's target labels, so code running in
// the worker cannot claim another Task's series.
const escape = value => String(value).replace(/\\/g, '\\\\').replace(/\n/g, '\\n').replace(/"/g, '\\"');
const labelText = labels => {
  const entries = Object.entries(labels);
  return entries.length ? `{${entries.map(([key, value]) => `${key}="${escape(value)}"`).join(',')}}` : '';
};

export function createRegistry() {
  const metrics = [];
  const series = (values, labels) => {
    const key = JSON.stringify(labels);
    if (!values.has(key)) values.set(key, {labels, value: 0});
    return values.get(key);
  };
  return {
    counter(name, help) {
      const values = new Map();
      metrics.push(() => [`# HELP ${name} ${help}`, `# TYPE ${name} counter`,
        ...[...values.values()].map(({labels, value}) => `${name}${labelText(labels)} ${value}`)]);
      return {inc: (labels = {}, amount = 1) => { if (amount > 0) series(values, labels).value += amount; }};
    },
    // collect returns the current value of a counter kept elsewhere.
    collectedCounter(name, help, collect) {
      metrics.push(() => [`# HELP ${name} ${help}`, `# TYPE ${name} counter`, `${name} ${collect()}`]);
    },
    histogram(name, help, buckets) {
      const values = new Map();
      metrics.push(() => [`# HELP ${name} ${help}`, `# TYPE ${name} histogram`,
        ...[...values.values()].flatMap(({labels, counts, sum, count}) => [
          ...buckets.map((bound, i) => `${name}_bucket${labelText({...labels, le: bound})} ${counts[i]}`),
          `${name}_bucket${labelText({...labels, le: '+Inf'})} ${count}`,
          `${name}_sum${labelText(labels)} ${sum}`, `${name}_count${labelText(labels)} ${count}`])]);
      return {observe(labels, value) {
        const key = JSON.stringify(labels);
        if (!values.has(key)) values.set(key, {labels, counts: buckets.map(() => 0), sum: 0, count: 0});
        const entry = values.get(key);
        buckets.forEach((bound, i) => { if (value <= bound) entry.counts[i]++; });
        entry.sum += value;
        entry.count++;
      }};
    },
    text: () => metrics.flatMap(render => render()).join('\n') + '\n',
  };
}

export const registry = createRegistry();

export const tokens = registry.counter('agent_worker_tokens_total',
  'Model tokens used by CLI turns, by backend, model and kind.');
export const turnDuration = registry.histogram('agent_worker_turn_duration_seconds',
  'Duration of CLI turns by backend, model and outcome.', [5, 15, 30, 60, 120, 300, 600, 1200, 1800, 3600]);
export const mcpRequestDuration = registry.histogram('agent_worker_mcp_request_duration_seconds',
  'Duration of MCP requests forwarded with a ServiceAccount token, by server and outcome.',
  [0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60]);

// Idle time accrues from readiness or the last turn to the next turn and is
// read at scrape time, so a worker that exits between scrapes loses little.
let idleSince = null;
let idleTotal = 0;
const now = () => Date.now() / 1000;
export const idle = {
  start() { if (idleSince === null) idleSince = now(); },
  stop() { if (idleSince !== null) { idleTotal += now() - idleSince; idleSince = null; } },
};
registry.collectedCounter('agent_worker_idle_seconds_total', 'Time the worker spent ready without a running turn.',
  () => idleTotal + (idleSince === null ? 0 : now() - idleSince));

let ready = false;
export const setReady = value => { ready = value; };

export function serveMetrics(port = 9464) {
  const server = http.createServer((req, res) => {
    if (req.method !== 'GET') { res.writeHead(405).end(); return; }
    if (req.url === '/metrics') {
      res.writeHead(200, {'content-type': 'text/plain; version=0.0.4'}).end(registry.text());
    } else if (req.url === '/readyz') {
      res.writeHead(ready ? 200 : 503).end(ready ? 'ready' : 'starting');
    } else res.writeHead(404).end();
  });
  server.listen(port);
  server.unref();
  return server;
}
