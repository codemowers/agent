window.installSidebarResize = function installSidebarResize(separator, sidebar, mobile) {
  let width;
  try { width = Number(localStorage.getItem('agent-operator-sidebar-width')) || undefined; } catch {}
  function limits() {
    const minimum = parseFloat(getComputedStyle(document.documentElement).fontSize) * 30;
    return {minimum, maximum: Math.max(minimum, document.querySelector('main').clientWidth - 326)};
  }
  function apply(value, save = false) {
    if (mobile.matches || document.body.classList.contains('read-only')) return;
    const {minimum, maximum} = limits();
    width = Math.round(Math.min(maximum, Math.max(minimum, value)));
    document.documentElement.style.setProperty('--sidebar-width', `${width}px`);
    for (const [key, value] of Object.entries({valuemin: minimum, valuemax: maximum, valuenow: width})) {
      separator.setAttribute('aria-' + key, String(Math.round(value)));
    }
    if (save) { try { localStorage.setItem('agent-operator-sidebar-width', String(width)); } catch {} }
  }
  separator.addEventListener('pointerdown', event => {
    if (event.button !== 0 || mobile.matches || document.body.classList.contains('read-only')) return;
    event.preventDefault();
    separator.setPointerCapture(event.pointerId);
    document.body.classList.add('sidebar-resizing');
  });
  separator.addEventListener('pointermove', event => {
    if (separator.hasPointerCapture(event.pointerId)) apply(event.clientX - sidebar.getBoundingClientRect().left);
  });
  const finish = () => {
    document.body.classList.remove('sidebar-resizing');
    if (width) apply(width, true);
  };
  separator.addEventListener('lostpointercapture', finish);
  for (const name of ['pointerup', 'pointercancel']) separator.addEventListener(name, event => {
    if (separator.hasPointerCapture(event.pointerId)) separator.releasePointerCapture(event.pointerId);
  });
  separator.addEventListener('keydown', event => {
    const {minimum, maximum} = limits();
    const current = sidebar.getBoundingClientRect().width;
    const value = {ArrowLeft: current - 20, ArrowRight: current + 20, Home: minimum, End: maximum}[event.key];
    if (value === undefined) return;
    event.preventDefault(); apply(value, true);
  });
  const refresh = () => apply(width || sidebar.getBoundingClientRect().width);
  window.addEventListener('resize', refresh); mobile.addEventListener('change', refresh);
  refresh();
}
