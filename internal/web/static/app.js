// YAMP page helpers. No inline scripts: the CSP allows only same-origin files.
(() => {
  'use strict';

  // Forms with data-confirm ask before they submit.
  document.addEventListener('submit', (e) => {
    const msg = e.target.dataset && e.target.dataset.confirm;
    if (msg && !window.confirm(msg)) {
      e.preventDefault();
      e.stopImmediatePropagation();
    }
  }, true);

  // In discreet mode, the eye button shows all names until the next page.
  document.addEventListener('click', (e) => {
    const btn = e.target.closest('[data-reveal]');
    if (!btn) return;
    const on = document.body.classList.toggle('reveal');
    btn.setAttribute('aria-pressed', on ? 'true' : 'false');
  });
})();
