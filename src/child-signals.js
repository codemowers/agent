// User-namespace/AppArmor confinement may deny signaling a process group even
// when signaling our own child PID is allowed. Expected denial/disappearance
// must not escape a SIGTERM callback and create another repair task.
export function signalChild(child, signal, {kill = process.kill.bind(process),
  denied = event => console.warn(JSON.stringify({event: 'worker_signal_denied', ...event}))} = {}) {
  if (!child?.pid || child.exitCode !== null || child.signalCode !== null) return false;
  try { kill(-child.pid, signal); return true; }
  catch (error) {
    if (!['ESRCH', 'EACCES', 'EPERM'].includes(error.code)) throw error;
  }
  try { kill(child.pid, signal); return true; }
  catch (error) {
    if (error.code === 'ESRCH') return false;
    if (!['EACCES', 'EPERM'].includes(error.code)) throw error;
    denied({signal, code: error.code}); return false;
  }
}
