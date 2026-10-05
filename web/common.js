'use strict';

// Helpers shared by the downlink (app.js) and uplink (uplink.js) pages.

const $ = (id) => document.getElementById(id);

// ---------------------------------------------------------------------------
// Small canvas time-series chart (no dependencies).
// ---------------------------------------------------------------------------
const PALETTE = ['#2f6fdb', '#e8703a', '#1f9e74', '#b04fc9', '#d4a21a', '#6b6b70'];

class Chart {
  constructor(canvas, series, { windowSec = 90 } = {}) {
    this.canvas = canvas;
    this.series = series.map((s, i) => ({ color: PALETTE[i % PALETTE.length], ...s, pts: [] }));
    this.windowSec = windowSec;
  }
  push(t, values) {
    for (const s of this.series) {
      const v = values[s.key];
      if (v !== undefined && v !== null && Number.isFinite(v)) s.pts.push([t, v]);
      const cutoff = t - this.windowSec * 1000;
      while (s.pts.length && s.pts[0][0] < cutoff) s.pts.shift();
    }
  }
  draw(now) {
    const c = this.canvas;
    const dpr = window.devicePixelRatio || 1;
    const w = c.clientWidth, h = c.clientHeight;
    if (c.width !== w * dpr || c.height !== h * dpr) { c.width = w * dpr; c.height = h * dpr; }
    const ctx = c.getContext('2d');
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, h);
    const css = getComputedStyle(document.documentElement);
    const grid = css.getPropertyValue('--grid').trim();
    const muted = css.getPropertyValue('--muted').trim();
    const L = 44, R = 8, T = 22, B = 18;
    const t1 = now, t0 = now - this.windowSec * 1000;
    let max = 0;
    for (const s of this.series) for (const [, v] of s.pts) max = Math.max(max, v);
    max = niceMax(max || 1);
    const x = (t) => L + (t - t0) / (t1 - t0) * (w - L - R);
    const y = (v) => T + (1 - v / max) * (h - T - B);
    ctx.font = '11px system-ui, sans-serif';
    ctx.lineWidth = 1;
    for (let i = 0; i <= 4; i++) {
      const v = max * i / 4, yy = y(v);
      ctx.strokeStyle = grid; ctx.beginPath(); ctx.moveTo(L, yy); ctx.lineTo(w - R, yy); ctx.stroke();
      ctx.fillStyle = muted; ctx.textAlign = 'right'; ctx.textBaseline = 'middle';
      ctx.fillText(fmtNum(v), L - 6, yy);
    }
    ctx.textAlign = 'left'; ctx.textBaseline = 'alphabetic';
    ctx.fillText(`last ${this.windowSec}s`, L, h - 4);
    let lx = L;
    for (const s of this.series) {
      ctx.fillStyle = s.color; ctx.fillRect(lx, 7, 10, 3);
      ctx.fillStyle = muted; ctx.textBaseline = 'middle';
      const label = s.name; ctx.fillText(label, lx + 14, 9);
      lx += 14 + ctx.measureText(label).width + 14;
      if (s.pts.length < 2) continue;
      ctx.strokeStyle = s.color; ctx.lineWidth = 1.75; ctx.beginPath();
      s.pts.forEach(([t, v], i) => (i ? ctx.lineTo(x(t), y(v)) : ctx.moveTo(x(t), y(v))));
      ctx.stroke();
    }
  }
}

function niceMax(v) {
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 2.5, 5, 10]) if (v <= m * p) return m * p;
  return 10 * p;
}
function fmtNum(v) {
  if (v === null || v === undefined || Number.isNaN(v)) return '–';
  if (typeof v !== 'number') return String(v);
  if (Math.abs(v) >= 1e6) return (v / 1e6).toFixed(1) + 'M';
  if (Math.abs(v) >= 1e4) return (v / 1e3).toFixed(0) + 'k';
  if (Math.abs(v) >= 100 || Number.isInteger(v)) return v.toFixed(0);
  if (Math.abs(v) >= 1) return v.toFixed(1);
  return v.toFixed(3);
}

function kvHTML(obj, prefix = '') {
  const rows = [];
  const walk = (o, p) => {
    for (const [k, v] of Object.entries(o || {})) {
      if (v && typeof v === 'object' && !Array.isArray(v)) walk(v, p + k + '.');
      else if (v !== undefined) rows.push(`<div><span title="${p + k}">${p + k}</span><span>${typeof v === 'number' ? fmtNum(v) : String(v)}</span></div>`);
    }
  };
  walk(obj, prefix);
  return rows.join('');
}

function tilesHTML(tiles) {
  return tiles.map(([label, val, unit]) =>
    `<div class="tile"><div class="label">${label}</div><div class="value">${typeof val === 'string' ? val : fmtNum(val)} <small>${unit}</small></div></div>`).join('');
}

function delta(cur, prev, key) {
  if (!cur || !prev || cur[key] === undefined || prev[key] === undefined) return undefined;
  return cur[key] - prev[key];
}

// ---------------------------------------------------------------------------
// Signaling / transport helpers
// ---------------------------------------------------------------------------
function waitForGathering(pc, timeoutMs) {
  if (pc.iceGatheringState === 'complete') return Promise.resolve();
  return new Promise((resolve) => {
    const t = setTimeout(resolve, timeoutMs);
    pc.addEventListener('icegatheringstatechange', () => {
      if (pc.iceGatheringState === 'complete') { clearTimeout(t); resolve(); }
    });
  });
}

// Device and network fields common to both pages' client metadata.
function deviceInfo() {
  const c = navigator.connection || {};
  return {
    userAgent: navigator.userAgent,
    userAgentData: navigator.userAgentData ? { brands: navigator.userAgentData.brands, platform: navigator.userAgentData.platform, mobile: navigator.userAgentData.mobile } : null,
    hardwareConcurrency: navigator.hardwareConcurrency,
    deviceMemory: navigator.deviceMemory,
    screen: { w: screen.width, h: screen.height, dpr: window.devicePixelRatio },
    viewport: { w: innerWidth, h: innerHeight },
    connection: { effectiveType: c.effectiveType, downlink: c.downlink, rtt: c.rtt, type: c.type, saveData: c.saveData },
    timeOrigin: performance.timeOrigin,
  };
}

function sendOnChannel(dc, obj) {
  if (!dc || dc.readyState !== 'open') return;
  if (dc.bufferedAmount > 4 * 1024 * 1024) return; // don't pile up if the uplink is slow
  const s = JSON.stringify(obj);
  if (s.length > 240 * 1024) {
    // Stay under the SCTP max-message-size browsers negotiate (256 KiB).
    delete obj.raw;
    obj.rawDropped = true;
    dc.send(JSON.stringify(obj));
    return;
  }
  dc.send(s);
}

function downloadJSON(filename, obj) {
  const blob = new Blob([JSON.stringify(obj)], { type: 'application/json' });
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = filename;
  a.click();
  setTimeout(() => URL.revokeObjectURL(a.href), 1000);
}

// ---------------------------------------------------------------------------
// Client/server clock offset over the DataChannel (NTP-style). offsetMs is
// server minus client wall clock; the sample with the lowest RTT in the last
// 10 s wins, so its error is at most rttMs/2.
// ---------------------------------------------------------------------------
class ClockSync {
  constructor(send, onChange) {
    this.send = send;
    this.onChange = onChange;
    this.samples = [];
    this.best = null;
    this.nextId = 1;
    this.timer = null;
  }
  start(intervalMs = 1000) {
    this.ping();
    this.timer = setInterval(() => this.ping(), intervalMs);
  }
  stop() { clearInterval(this.timer); this.timer = null; }
  // Wall clock, like the server's and Chrome's RTCP NTP timestamps.
  // performance.timeOrigin + now() is monotonic and drifts from it by ms.
  static now() { return Date.now(); }
  ping() { this.send({ type: 'clock-ping', id: this.nextId++, t1: ClockSync.now() }); }
  onPong(msg) {
    const t3 = ClockSync.now();
    const rttMs = t3 - msg.t1;
    const s = { at: t3, rttMs, offsetMs: msg.t2 - (msg.t1 + t3) / 2 };
    this.samples.push(s);
    this.samples = this.samples.filter((x) => t3 - x.at <= 10000);
    const best = this.samples.reduce((a, b) => (b.rttMs < a.rttMs ? b : a));
    const changed = !this.best || best.offsetMs !== this.best.offsetMs;
    this.best = best;
    if (changed) {
      this.send({ type: 'clock-sync', offsetMs: best.offsetMs, rttMs: best.rttMs, samples: this.samples.length });
      this.onChange && this.onChange(best);
    }
  }
}

// ---------------------------------------------------------------------------
// Event log. state needs {events, pendingEvents, t0Wall}; listEl is an <ol>.
// ---------------------------------------------------------------------------
function makeEventLogger(state, listEl) {
  return function logEvent(src, kind, value, extra) {
    const ev = { t: Date.now(), perf: performance.now(), src, kind, value, ...extra };
    state.events.push(ev);
    if (src === 'client') state.pendingEvents.push(ev);
    const li = document.createElement('li');
    const rel = state.t0Wall ? ((ev.t - state.t0Wall) / 1000).toFixed(3) + 's' : '';
    li.textContent = `${rel} [${src}] ${kind}: ${typeof value === 'object' ? JSON.stringify(value) : value}`;
    if (src === 'server') li.className = 'server';
    if (/fail|stall|error|closed|freeze|waiting/i.test(`${kind} ${value}`)) li.classList.add('bad');
    listEl.appendChild(li);
    listEl.scrollTop = listEl.scrollHeight;
  };
}
