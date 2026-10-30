/* Shared UI runtime: icons, formatting, toasts, theme, clipboard, API helper,
   sparklines and a line chart with crosshair + tooltip. No dependencies. */
(function () {
  'use strict';
  // Where this script was served from: the token logos sit next to it.
  const ASSETS = ((document.currentScript && document.currentScript.src) || '').replace(/[^/]*$/, '');
  // token returns an <img> of a coin's logo (ARG, uARG, wARG, and the
  // bridge's USDC and USDT), or '' for coins without one, so callers can
  // fall back to a lettered mark.
  const TOKENS = { arg: 1, uarg: 1, warg: 1, usdc: 1, usdt: 1 };
  function token(symbol, cls) {
    const k = String(symbol || '').toLowerCase();
    if (!TOKENS[k]) return '';
    return '<img class="tok' + (cls ? ' ' + cls : '') + '" src="' + ASSETS + 'tokens/' + k + '.svg" alt="" width="36" height="36" decoding="async">';
  }
  // chain returns an <img> of an EVM chain's logo by chain id (mainnets and
  // their test networks), or '' for a chain without one.
  const CHAINS = { 1: 'ethereum', 11155111: 'ethereum', 17000: 'ethereum', 560048: 'ethereum', 8453: 'base', 84532: 'base', 42161: 'arbitrum', 421614: 'arbitrum', 56: 'bnb', 97: 'bnb' };
  function chain(id, cls) {
    const k = CHAINS[Number(id)];
    if (!k) return '';
    return '<img class="chainlogo' + (cls ? ' ' + cls : '') + '" src="' + ASSETS + 'tokens/chain-' + k + '.svg" alt="" width="36" height="36" decoding="async">';
  }

  // ---- icons: 24x24 stroke paths (Lucide-compatible geometry) ----
  const paths = {
    menu: 'M4 6h16M4 12h16M4 18h16',
    play: 'M6 4l14 8-14 8V4z',
    stop: 'M5 5h14v14H5z',
    wallet: 'M20 12V8H6a2 2 0 0 1 0-4h12v4M4 6v12a2 2 0 0 0 2 2h14v-6M18 12a2 2 0 1 0 0 4 2 2 0 0 0 0-4z',
    send: 'M22 2 11 13M22 2l-7 20-4-9-9-4 20-7z',
    receive: 'M12 3v12m0 0 5-5m-5 5-5-5M4 19h16',
    copy: 'M8 8h12v12H8zM16 8V6a2 2 0 0 0-2-2H6a2 2 0 0 0-2 2v8a2 2 0 0 0 2 2h2',
    check: 'M20 6 9 17l-5-5',
    x: 'M18 6 6 18M6 6l12 12',
    alert: 'M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0zM12 9v4m0 4h.01',
    info: 'M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20zM12 16v-4m0-4h.01',
    chevronRight: 'm9 18 6-6-6-6',
    chevronDown: 'm6 9 6 6 6-6',
    settings: 'M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM19.4 15a1.7 1.7 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.8-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-1.1-1.5 1.7 1.7 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.7 1.7 0 0 0 .3-1.8 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.7 1.7 0 0 0 1.5-1.1 1.7 1.7 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.7 1.7 0 0 0 1.8.3H9a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.8V9a1.7 1.7 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.7 1.7 0 0 0-1.5 1z',
    activity: 'M22 12h-4l-3 9L9 3l-3 9H2',
    server: 'M4 2h16a2 2 0 0 1 2 2v4a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2zm0 12h16a2 2 0 0 1 2 2v4a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2v-4a2 2 0 0 1 2-2zM6 6h.01M6 18h.01',
    users: 'M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2M9 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8zm14 10v-2a4 4 0 0 0-3-3.9M16 3.1a4 4 0 0 1 0 7.8',
    cube: 'M21 16V8a2 2 0 0 0-1-1.7l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.7l7 4a2 2 0 0 0 2 0l7-4a2 2 0 0 0 1-1.7zM3.3 7l8.7 5 8.7-5M12 22V12',
    hash: 'M4 9h16M4 15h16M10 3 8 21M16 3l-2 18',
    search: 'M11 19a8 8 0 1 0 0-16 8 8 0 0 0 0 16zm10 2-4.3-4.3',
    sun: 'M12 17a5 5 0 1 0 0-10 5 5 0 0 0 0 10zM12 1v2m0 18v2M4.2 4.2l1.4 1.4m12.8 12.8 1.4 1.4M1 12h2m18 0h2M4.2 19.8l1.4-1.4M18.4 5.6l1.4-1.4',
    moon: 'M21 12.8A9 9 0 1 1 11.2 3 7 7 0 0 0 21 12.8z',
    external: 'M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6M15 3h6v6M10 14 21 3',
    refresh: 'M21 12a9 9 0 1 1-2.6-6.4M21 3v6h-6',
    lock: 'M5 11h14a2 2 0 0 1 2 2v7a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-7a2 2 0 0 1 2-2zm2 0V7a5 5 0 0 1 10 0v4',
    unlock: 'M5 11h14a2 2 0 0 1 2 2v7a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-7a2 2 0 0 1 2-2zm2 0V7a5 5 0 0 1 9.9-1',
    eye: 'M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8zm11 3a3 3 0 1 0 0-6 3 3 0 0 0 0 6z',
    eyeOff: 'M17.9 17.9A10 10 0 0 1 12 20c-7 0-11-8-11-8a18 18 0 0 1 5.1-5.9m3.8-1.9A9 9 0 0 1 12 4c7 0 11 8 11 8a18 18 0 0 1-2.2 3.2M1 1l22 22M9.9 9.9a3 3 0 1 0 4.2 4.2',
    pickaxe: 'M14.5 2.5 21.5 9.5M18 6 3 21M13 4a9 9 0 0 1 7 7M11 6a7 7 0 0 1 7 7',
    shield: 'M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z',
    history: 'M3 12a9 9 0 1 0 3-6.7M3 3v6h6M12 7v5l4 2',
    layers: 'm12 2 10 5-10 5L2 7l10-5zM2 12l10 5 10-5M2 17l10 5 10-5',
    download: 'M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4M7 10l5 5 5-5M12 15V3',
    key: 'm21 2-2 2m-7.6 7.6a5.5 5.5 0 1 1-7.8 7.8 5.5 5.5 0 0 1 7.8-7.8zm0 0L15.5 7.5m0 0 3 3L22 7l-3-3-3.5 3.5',
    zap: 'M13 2 3 14h9l-1 8 10-12h-9l1-8z',
    globe: 'M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20zM2 12h20M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z',
    arrowUpRight: 'M7 17 17 7M7 7h10v10',
    arrowDownLeft: 'M17 7 7 17M17 17H7V7',
    cpu: 'M5 5h14v14H5zM9 9h6v6H9zM9 1v4m6-4v4M9 19v4m6-4v4M1 9h4m-4 6h4m14-6h4m-4 6h4',
    swap: 'm21 16-4 4-4-4M17 20V4M3 8l4-4 4 4M7 4v16',
    arrowDown: 'M12 5v14m7-7-7 7-7-7',
    chevronLeft: 'm15 18-6-6 6-6',
    share: 'M4 12v7a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-7M16 6l-4-4-4 4M12 2v13',
    user: 'M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2M12 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8z',
    clipboard: 'M9 4h6a1 1 0 0 1 1 1v1a1 1 0 0 1-1 1H9a1 1 0 0 1-1-1V5a1 1 0 0 1 1-1zM16 5h2a2 2 0 0 1 2 2v13a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2h2',
  };
  function icon(name, cls) {
    const d = paths[name] || paths.info;
    return '<svg class="icon ' + (cls || '') + '" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="' + d + '"/></svg>';
  }
  function mountIcons(root) {
    (root || document).querySelectorAll('i[data-icon]').forEach(el => {
      el.outerHTML = icon(el.dataset.icon, el.className);
    });
  }

  // ---- formatting ----
  const nf = new Intl.NumberFormat(undefined, { maximumFractionDigits: 0 });
  const fmt = {
    int: n => nf.format(Math.round(Number(n) || 0)),
    coin: (units, digits) => {
      const neg = Number(units) < 0; let u = Math.abs(Number(units) || 0);
      const whole = Math.floor(u / 1e8), frac = Math.round(u % 1e8);
      let s = String(whole).replace(/\B(?=(\d{3})+(?!\d))/g, ',');
      let f = String(frac).padStart(8, '0');
      if (digits === undefined) f = f.replace(/0+$/, ''); else f = f.slice(0, digits);
      return (neg ? '-' : '') + s + (f ? '.' + f : '');
    },
    hashrate: h => { const u = ['H/s', 'kH/s', 'MH/s', 'GH/s', 'TH/s']; let i = 0; h = Number(h) || 0; while (h >= 1000 && i < u.length - 1) { h /= 1000; i++; } return (i === 0 ? h.toFixed(h < 10 ? 1 : 0) : h.toFixed(2)) + ' ' + u[i]; },
    bytes: b => { const u = ['B', 'KB', 'MB', 'GB']; let i = 0; b = Number(b) || 0; while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; } return (i === 0 ? b : b.toFixed(1)) + ' ' + u[i]; },
    ago: ts => { const s = Math.max(0, Math.floor(Date.now() / 1000 - ts)); if (s < 60) return s + 's ago'; if (s < 3600) return Math.floor(s / 60) + 'm ago'; if (s < 172800) return Math.floor(s / 3600) + 'h ago'; return Math.floor(s / 86400) + 'd ago'; },
    when: ts => new Date(ts * 1000).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' }),
    short: (s, head, tail) => { s = String(s || ''); head = head || 10; tail = tail === undefined ? 6 : tail; return s.length > head + tail + 1 ? s.slice(0, head) + '…' + (tail ? s.slice(-tail) : '') : s; },
    duration: s => { s = Math.max(0, Math.floor(s)); const h = Math.floor(s / 3600), m = Math.floor(s % 3600 / 60); return h ? h + 'h ' + m + 'm' : m + 'm ' + (s % 60) + 's'; },
  };

  // ---- toasts ----
  function toast(message, kind, ms) {
    let host = document.querySelector('.toasts');
    if (!host) { host = document.createElement('div'); host.className = 'toasts'; host.setAttribute('role', 'status'); host.setAttribute('aria-live', 'polite'); document.body.appendChild(host); }
    const el = document.createElement('div');
    el.className = 'toast ' + (kind || '');
    el.innerHTML = icon(kind === 'ok' ? 'check' : kind === 'danger' ? 'alert' : kind === 'warn' ? 'alert' : 'info') + '<div>' + escapeHTML(message) + '</div>';
    host.appendChild(el);
    setTimeout(() => { el.style.opacity = '0'; el.style.transition = 'opacity .2s'; setTimeout(() => el.remove(), 220); }, ms || 3600);
  }
  function escapeHTML(s) { return String(s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])); }

  // ---- theme ----
  const theme = {
    current: () => document.documentElement.dataset.theme || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'),
    set: t => { document.documentElement.dataset.theme = t; try { localStorage.setItem('theme', t); } catch (e) { } document.querySelectorAll('[data-theme-toggle]').forEach(b => { b.innerHTML = icon(t === 'dark' ? 'sun' : 'moon'); b.setAttribute('aria-label', t === 'dark' ? 'Switch to light theme' : 'Switch to dark theme'); }); },
    toggle: () => theme.set(theme.current() === 'dark' ? 'light' : 'dark'),
    // Only a theme the user picked (or ?theme=) is stored; otherwise the page follows the device.
    init: () => { document.querySelectorAll('[data-theme-toggle]').forEach(b => { b.onclick = theme.toggle; }); let forced = null, stored = null; try { forced = new URLSearchParams(location.search).get('theme'); stored = localStorage.getItem('theme'); } catch (e) { } const pick = forced === 'light' || forced === 'dark' ? forced : stored === 'light' || stored === 'dark' ? stored : ''; if (pick) theme.set(pick); else { const t = theme.current(); document.querySelectorAll('[data-theme-toggle]').forEach(b => { b.innerHTML = icon(t === 'dark' ? 'sun' : 'moon'); b.setAttribute('aria-label', t === 'dark' ? 'Switch to light theme' : 'Switch to dark theme'); }); } },
  };

  // ---- clipboard ----
  async function copy(text, label) {
    try { await navigator.clipboard.writeText(text); toast((label || 'Copied') + ' to clipboard', 'ok', 1800); return true; }
    catch (e) { toast('Copy failed. Select the text and copy it manually.', 'warn'); return false; }
  }

  // ---- API helper ----
  async function api(url, body, headers) {
    const r = await fetch(url, { method: body === undefined ? 'GET' : 'POST', headers: Object.assign({ 'Content-Type': 'application/json' }, headers || {}), body: body === undefined ? undefined : JSON.stringify(body) });
    let j = null; try { j = await r.json(); } catch (e) { }
    if (!r.ok) throw new Error((j && (j.error || j.message)) || r.statusText || 'Request failed');
    return j;
  }

  // ---- sparkline ----
  function spark(el, values) {
    const w = 200, h = 36, p = 2;
    if (!values || values.length < 2) { el.innerHTML = ''; return; }
    const min = Math.min(...values), max = Math.max(...values), span = max - min || 1;
    const pts = values.map((v, i) => [p + i * (w - 2 * p) / (values.length - 1), h - p - (v - min) / span * (h - 2 * p)]);
    el.innerHTML = '<svg class="spark" viewBox="0 0 ' + w + ' ' + h + '" preserveAspectRatio="none" aria-hidden="true"><path d="M' + pts.map(q => q[0].toFixed(1) + ' ' + q[1].toFixed(1)).join('L') + '"/></svg>';
  }

  // ---- line chart: one series, recessive grid, crosshair + tooltip ----
  function chart(el, points, opts) {
    opts = opts || {};
    const W = 640, H = opts.height || 220, m = { t: 12, r: 12, b: 26, l: 48 };
    el.classList.add('chart');
    if (!points || points.length < 2) { el.innerHTML = '<div class="empty"><p>' + escapeHTML(opts.empty || 'Not enough data yet.') + '</p></div>'; return; }
    const xs = points.map(p => p.x), ys = points.map(p => p.y);
    const x0 = Math.min(...xs), x1 = Math.max(...xs); let y0 = Math.min(...ys), y1 = Math.max(...ys);
    if (opts.zero) y0 = Math.min(0, y0);
    if (y1 === y0) { y1 += 1; y0 -= 1; }
    const pad = (y1 - y0) * 0.08; y1 += pad; if (!opts.zero) y0 -= pad;
    const sx = x => m.l + (x - x0) / (x1 - x0 || 1) * (W - m.l - m.r);
    const sy = y => m.t + (1 - (y - y0) / (y1 - y0)) * (H - m.t - m.b);
    const ticks = niceTicks(y0, y1, 4);
    const fy = opts.formatY || (v => fmt.int(v));
    const fx = opts.formatX || (v => String(v));
    let grid = '', axis = '';
    ticks.forEach(t => { const y = sy(t); grid += '<line x1="' + m.l + '" x2="' + (W - m.r) + '" y1="' + y + '" y2="' + y + '"/>'; axis += '<text x="' + (m.l - 8) + '" y="' + (y + 4) + '" text-anchor="end">' + escapeHTML(fy(t)) + '</text>'; });
    const nx = 5;
    for (let i = 0; i <= nx; i++) { const x = x0 + (x1 - x0) * i / nx; axis += '<text x="' + sx(x) + '" y="' + (H - 6) + '" text-anchor="' + (i === 0 ? 'start' : i === nx ? 'end' : 'middle') + '">' + escapeHTML(fx(x)) + '</text>'; }
    const d = points.map((p, i) => (i ? 'L' : 'M') + sx(p.x).toFixed(1) + ' ' + sy(p.y).toFixed(1)).join('');
    const area = d + 'L' + sx(x1).toFixed(1) + ' ' + sy(y0) + 'L' + sx(x0).toFixed(1) + ' ' + sy(y0) + 'Z';
    el.innerHTML = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" aria-label="' + escapeHTML(opts.label || 'chart') + '">' +
      '<g class="grid">' + grid + '</g><line class="baseline" x1="' + m.l + '" x2="' + (W - m.r) + '" y1="' + sy(y0) + '" y2="' + sy(y0) + '"/>' +
      '<g class="axis">' + axis + '</g>' + (opts.area === false ? '' : '<path class="area" d="' + area + '"/>') + '<path class="line" d="' + d + '"/>' +
      '<line class="crosshair" y1="' + m.t + '" y2="' + (H - m.b) + '" style="display:none"/><circle class="marker" r="4" style="display:none"/>' +
      '<rect class="hit" x="' + m.l + '" y="' + m.t + '" width="' + (W - m.l - m.r) + '" height="' + (H - m.t - m.b) + '"/></svg><div class="chart-tip"></div>';
    const svg = el.querySelector('svg'), hit = el.querySelector('.hit'), cross = el.querySelector('.crosshair'), marker = el.querySelector('.marker'), tip = el.querySelector('.chart-tip');
    function show(evt) {
      const rect = svg.getBoundingClientRect(); const px = (evt.clientX - rect.left) / rect.width * W;
      const xv = x0 + (px - m.l) / (W - m.l - m.r) * (x1 - x0);
      let best = 0, bd = Infinity; points.forEach((p, i) => { const dd = Math.abs(p.x - xv); if (dd < bd) { bd = dd; best = i; } });
      const p = points[best], cx = sx(p.x), cy = sy(p.y);
      cross.setAttribute('x1', cx); cross.setAttribute('x2', cx); cross.style.display = '';
      marker.setAttribute('cx', cx); marker.setAttribute('cy', cy); marker.style.display = '';
      tip.innerHTML = '<span class="muted">' + escapeHTML(fx(p.x)) + '</span> &nbsp; <b>' + escapeHTML((opts.formatTip || fy)(p.y)) + '</b>';
      tip.style.left = (cx / W * rect.width) + 'px'; tip.style.top = (cy / H * rect.height) + 'px'; tip.style.opacity = '1';
    }
    function hide() { cross.style.display = 'none'; marker.style.display = 'none'; tip.style.opacity = '0'; }
    hit.addEventListener('mousemove', show); hit.addEventListener('mouseleave', hide);
    hit.addEventListener('touchstart', e => { show(e.touches[0]); }, { passive: true }); hit.addEventListener('touchmove', e => { show(e.touches[0]); }, { passive: true }); hit.addEventListener('touchend', hide);
  }
  function niceTicks(lo, hi, n) {
    const span = hi - lo, raw = span / n, mag = Math.pow(10, Math.floor(Math.log10(raw))), norm = raw / mag;
    const step = (norm < 1.5 ? 1 : norm < 3 ? 2 : norm < 7 ? 5 : 10) * mag;
    const out = []; for (let v = Math.ceil(lo / step) * step; v <= hi + 1e-9; v += step) out.push(+v.toFixed(10)); return out;
  }

  window.UI = { icon, mountIcons, fmt, toast, theme, copy, api, spark, chart, escapeHTML, token, chain };
  document.addEventListener('DOMContentLoaded', () => { mountIcons(); theme.init(); });
})();
