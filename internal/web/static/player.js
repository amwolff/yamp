// YAMP player: source check, resume, heartbeats, keyboard shortcuts and neutral
// Media Session data.
(() => {
  'use strict';
  const v = document.getElementById('player');
  if (!v) return;

  const pid = v.dataset.pid;
  const resumeAt = Number(v.dataset.resume || 0) / 1000;
  const label = document.getElementById('source-label');
  const unplayable = document.getElementById('unplayable');
  const sources = Array.from(v.querySelectorAll('source'));

  // Offer a transcode if no <source> type is supported or every source fails.
  const showUnplayable = () => { if (unplayable) unplayable.hidden = false; };
  if (!sources.some((s) => v.canPlayType(s.type) !== '')) showUnplayable();
  if (sources.length > 0) sources[sources.length - 1].addEventListener('error', showUnplayable);

  v.addEventListener('loadedmetadata', () => {
    if (resumeAt > 5 && resumeAt < v.duration - 5) v.currentTime = resumeAt;
    const cur = sources.find((s) => s.src === v.currentSrc);
    if (label && cur) label.textContent = cur.dataset.label;
  }, { once: true });

  // Lock screens and notifications show Media Session data: use the public ID.
  const setSession = () => {
    if (!('mediaSession' in navigator) || typeof MediaMetadata === 'undefined') return;
    navigator.mediaSession.metadata = new MediaMetadata({ title: pid, artist: 'YAMP', album: '' });
  };
  setSession();
  v.addEventListener('play', setSession);

  // Heartbeats: resume position and watched time.
  const bytes = new Uint8Array(12);
  crypto.getRandomValues(bytes);
  const playKey = Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
  const url = '/api/playback/' + encodeURIComponent(pid);
  let watched = 0;
  let last = null;

  v.addEventListener('timeupdate', () => {
    const t = v.currentTime;
    if (!v.paused && !v.seeking && last !== null) {
      const d = t - last;
      if (d > 0 && d < 2 * Math.max(1, v.playbackRate)) watched += d;
    }
    last = t;
  });
  v.addEventListener('seeking', () => { last = null; });

  const body = (ended) => {
    const b = new URLSearchParams({
      play: playKey,
      pos: String(Math.floor(v.currentTime * 1000)),
      watched: String(Math.floor(watched * 1000)),
      ended: ended ? '1' : '0',
    });
    watched = 0;
    return b;
  };
  const send = (ended) => {
    fetch(url, { method: 'POST', body: body(ended), credentials: 'same-origin', keepalive: true }).catch(() => {});
  };
  setInterval(() => { if (!v.paused) send(false); }, 10000);
  v.addEventListener('pause', () => { if (!v.ended) send(false); });
  v.addEventListener('ended', () => send(true));
  window.addEventListener('pagehide', () => {
    if (v.currentTime > 0 && navigator.sendBeacon) navigator.sendBeacon(url, body(v.ended));
  });

  // Shortcuts: space/k play/pause, f full screen, m mute, arrows ±10 s.
  const toggleFullscreen = () => {
    if (document.fullscreenElement) { document.exitFullscreen(); return; }
    if (v.requestFullscreen) v.requestFullscreen();
    else if (v.webkitEnterFullscreen) v.webkitEnterFullscreen();
  };
  document.addEventListener('keydown', (e) => {
    if (e.altKey || e.ctrlKey || e.metaKey) return;
    if (e.target.closest && e.target.closest('input, textarea, select, button, summary')) return;
    switch (e.key) {
      case ' ':
      case 'k':
        e.preventDefault();
        if (v.paused) v.play(); else v.pause();
        break;
      case 'f':
        e.preventDefault();
        toggleFullscreen();
        break;
      case 'm':
        v.muted = !v.muted;
        break;
      case 'ArrowLeft':
        if (document.activeElement === v) return; // native controls handle it
        v.currentTime = Math.max(0, v.currentTime - 10);
        break;
      case 'ArrowRight':
        if (document.activeElement === v) return;
        v.currentTime = Math.min(v.duration || v.currentTime + 10, v.currentTime + 10);
        break;
      default:
    }
  });
})();
