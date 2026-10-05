'use strict';

// Uplink test: send a generated (hidden) video and a tone to the server and
// collect sender-side stats here plus receiver-side stats from the server.

const S = {
  pc: null,
  dc: null,
  sessionId: null,
  t0Perf: 0,
  t0Wall: 0,
  timers: [],
  stopTickers: [],
  statsSamples: [],     // {t, perf, raw: [...reports], derived}
  frames: [],           // generated source frames {n, t, perf}
  events: [],
  serverSnaps: [],
  serverHello: null,
  prev: null,
  qoe: null,
  pendingFrames: [],
  pendingEvents: [],
  tick: 0,
  source: null,
  audio: null,
  clock: null,
};

const preview = $('preview');
const logEvent = makeEventLogger(S, $('events'));

const charts = {
  bitrate: new Chart($('c-bitrate'), [
    { key: 'sent', name: 'client video sent' },
    { key: 'target', name: 'encoder target' },
    { key: 'availOut', name: 'client avail. outgoing' },
    { key: 'serverRecv', name: 'server received' },
  ]),
  rtt: new Chart($('c-rtt'), [
    { key: 'iceRtt', name: 'client ICE RTT' },
    { key: 'rtcpRtt', name: 'client RTCP RTT' },
    { key: 'clockRtt', name: 'clock-sync RTT' },
  ]),
  fps: new Chart($('c-fps'), [
    { key: 'source', name: 'source (canvas)' },
    { key: 'encoded', name: 'encoded' },
    { key: 'serverRecv', name: 'server received' },
  ]),
  loss: new Chart($('c-loss'), [
    { key: 'remoteLoss', name: 'RR fraction lost' },
    { key: 'serverLoss', name: 'server packet loss' },
    { key: 'serverFrameLoss', name: 'server frames lost' },
  ]),
  delay: new Chart($('c-delay'), [
    { key: 'owd', name: 'one-way capture→server' },
    { key: 'owdWall', name: 'one-way (wall-clock sync)' },
    { key: 'delayVar', name: 'delay variation' },
    { key: 'encode', name: 'encode/frame' },
    { key: 'pacer', name: 'pacer/packet' },
    { key: 'assembly', name: 'server frame assembly' },
  ]),
  quality: new Chart($('c-quality'), [
    { key: 'height', name: 'sent height (px)' },
    { key: 'qp', name: 'avg QP' },
  ]),
};

function drawCharts() {
  const now = Date.now();
  for (const c of Object.values(charts)) c.draw(now);
}

// ---------------------------------------------------------------------------
// Worker-driven ticker: main-thread timers are throttled to 1 Hz in background
// tabs, worker timers are not, so the source keeps its frame rate.
// ---------------------------------------------------------------------------
const TICKER_SRC = `
let timer = null;
onmessage = (e) => {
  clearTimeout(timer);
  const interval = e.data;
  if (!interval) return;
  const t0 = performance.now();
  let n = 0;
  const step = () => {
    postMessage(n);
    n++;
    timer = setTimeout(step, Math.max(0, t0 + n * interval - performance.now()));
  };
  step();
};`;

function workerTicker(intervalMs, fn) {
  const url = URL.createObjectURL(new Blob([TICKER_SRC], { type: 'text/javascript' }));
  const w = new Worker(url);
  URL.revokeObjectURL(url);
  w.onmessage = (e) => fn(e.data);
  w.postMessage(intervalMs);
  return () => w.terminate();
}

// ---------------------------------------------------------------------------
// Fake video source: a canvas that is never attached to the page.
// ---------------------------------------------------------------------------
class FakeVideoSource {
  constructor(width, height, fps, contentHint) {
    this.w = width; this.h = height; this.fps = fps;
    this.canvas = document.createElement('canvas');
    this.canvas.width = width; this.canvas.height = height;
    this.ctx = this.canvas.getContext('2d', { alpha: false });
    this.stream = this.canvas.captureStream(0);
    this.track = this.stream.getVideoTracks()[0];
    if (contentHint) this.track.contentHint = contentHint;
    const nw = Math.round(width / 8), nh = Math.round(height / 8);
    this.noise = this.ctx.createImageData(nw, nh);
    this.label = '';
    this.n = 0;
    this.draw(); // a frame exists before negotiation
  }
  start(onFrame) {
    this.stop = workerTicker(1000 / this.fps, () => {
      this.draw();
      onFrame({ n: this.n, t: Date.now(), perf: performance.now() });
      this.n++;
    });
  }
  draw() {
    const { ctx, w, h, n } = this;
    // Scrolling colour bars.
    const bars = 8, bw = w / bars, shift = (n * 4) % w;
    for (let i = -1; i < bars; i++) {
      ctx.fillStyle = `hsl(${(i * 45 + n) % 360} 70% 50%)`;
      ctx.fillRect(Math.floor(i * bw + shift), 0, Math.ceil(bw) + 1, h);
    }
    // Gradient band.
    const g = ctx.createLinearGradient(0, 0, w, 0);
    g.addColorStop(0, '#000'); g.addColorStop(0.5, `hsl(${(n * 3) % 360} 80% 60%)`); g.addColorStop(1, '#fff');
    ctx.fillStyle = g;
    ctx.fillRect(0, h * 0.72, w, h * 0.12);
    // Bouncing box.
    const bs = Math.round(h / 6);
    const px = Math.abs(((n * 9) % (2 * (w - bs))) - (w - bs));
    const py = Math.abs(((n * 6) % (2 * (h - bs))) - (h - bs));
    ctx.fillStyle = '#fff'; ctx.fillRect(px, py, bs, bs);
    ctx.fillStyle = '#111'; ctx.fillRect(px + bs / 4, py + bs / 4, bs / 2, bs / 2);
    // Noise patch so the encoder always has some work.
    const d = this.noise.data;
    for (let i = 0; i < d.length; i += 4) {
      const v = (Math.random() * 255) | 0;
      d[i] = d[i + 1] = d[i + 2] = v; d[i + 3] = 255;
    }
    ctx.putImageData(this.noise, w - this.noise.width - 16, h - this.noise.height - 16);
    // Frame counter and wall clock.
    const fs = Math.round(h / 16);
    ctx.font = `${fs}px ui-monospace, Menlo, monospace`;
    ctx.textBaseline = 'top';
    const text = `frame ${n}  ${new Date().toISOString().slice(11, 23)}  ${this.label}`;
    ctx.fillStyle = 'rgba(0,0,0,.65)';
    ctx.fillRect(16, 16, ctx.measureText(text).width + 24, fs * 1.5);
    ctx.fillStyle = '#fff';
    ctx.fillText(text, 28, 16 + fs * 0.25);
    if (this.track.requestFrame) this.track.requestFrame();
  }
  close() {
    this.stop && this.stop();
    this.track.stop();
  }
}

// Synthetic audio: a quiet 220 Hz tone plus a 1 kHz beep in the first 100 ms
// of every second (same as scripts/make-standin.sh), looped from a 1 s buffer.
class FakeAudioSource {
  constructor() {
    this.ctx = new AudioContext({ sampleRate: 48000 });
    const sr = this.ctx.sampleRate;
    const buf = this.ctx.createBuffer(1, sr, sr);
    const d = buf.getChannelData(0);
    for (let i = 0; i < sr; i++) {
      const t = i / sr;
      d[i] = 0.1 * Math.sin(2 * Math.PI * 220 * t) + (t < 0.1 ? 0.5 * Math.sin(2 * Math.PI * 1000 * t) : 0);
    }
    this.node = this.ctx.createBufferSource();
    this.node.buffer = buf;
    this.node.loop = true;
    const dest = this.ctx.createMediaStreamDestination();
    this.node.connect(dest);
    this.node.start();
    this.stream = dest.stream;
    this.track = dest.stream.getAudioTracks()[0];
  }
  close() {
    this.track.stop();
    this.ctx.close();
  }
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------
document.addEventListener('visibilitychange', () => logEvent('client', 'visibility', document.visibilityState));
$('showPreview').onchange = () => {
  preview.classList.toggle('show', $('showPreview').checked);
  preview.srcObject = $('showPreview').checked && S.source ? S.source.stream : null;
  if (preview.srcObject) preview.play().catch(() => {});
};

// ---------------------------------------------------------------------------
// Start / stop
// ---------------------------------------------------------------------------
$('start').onclick = start;
$('stop').onclick = () => stop('user stop');
$('download').onclick = download;

function setStatus(s) { $('status').textContent = s; }

function settings() {
  const [w, h] = $('resolution').value.split('x').map(Number);
  return {
    codec: $('codec').value,
    width: w, height: h,
    fps: Math.min(60, Math.max(1, Number($('fps').value) || 30)),
    maxBitrateKbps: $('maxBitrate').value === '' ? null : Number($('maxBitrate').value),
    degradationPreference: $('degradation').value || null,
    contentHint: $('contentHint').value || null,
    audio: $('audio').checked,
    statsIntervalMs: Math.max(50, Number($('interval').value) || 250),
  };
}

function newQoE() {
  return {
    connectMs: null,           // Start click -> pc connected
    firstPacketSentMs: null,   // Start click -> first video bytes sent
    serverFirstFrameMs: null,  // Start click -> server's "first video frame" event received
    sourceFrames: 0,
    sourceLateTicks: 0,        // source ticks more than 2 frame intervals late
    lastSourceAt: null,
    heightTimeSum: 0,
    heightTimeDur: 0,
    lastHeightAt: null,
    lastHeight: null,
  };
}

function clientInfo() {
  return { ...deviceInfo(), startedAtWall: S.t0Wall, settings: settings() };
}

async function start() {
  $('start').disabled = true;
  $('download').disabled = false;
  Object.assign(S, {
    statsSamples: [], frames: [], events: [], serverSnaps: [], serverHello: null, prev: null,
    pendingFrames: [], pendingEvents: [], tick: 0, qoe: newQoE(), sessionId: null,
  });
  $('events').innerHTML = '';
  for (const c of Object.values(charts)) for (const s of c.series) s.pts = [];
  S.t0Perf = performance.now();
  S.t0Wall = Date.now();
  const cfg = settings();
  logEvent('client', 'start', new Date(S.t0Wall).toISOString());
  logEvent('client', 'settings', cfg);
  setStatus('connecting…');

  S.source = new FakeVideoSource(cfg.width, cfg.height, cfg.fps, cfg.contentHint);
  if (cfg.audio) {
    try { S.audio = new FakeAudioSource(); } catch (err) { logEvent('client', 'error', 'audio: ' + err.message); }
  }
  $('showPreview').onchange();

  const stun = $('stun').value.trim();
  const pc = new RTCPeerConnection({ iceServers: stun ? [{ urls: stun }] : [] });
  S.pc = pc;
  const vt = pc.addTransceiver(S.source.track, { direction: 'sendonly', streams: [S.source.stream] });
  preferCodec(vt, cfg.codec);
  if (S.audio) pc.addTransceiver(S.audio.track, { direction: 'sendonly', streams: [S.source.stream] });
  const dc = pc.createDataChannel('telemetry', { ordered: true });
  S.dc = dc;
  S.clock = new ClockSync(sendToServer, (best) => logEvent('client', 'clockSync', `offset ${best.offsetMs.toFixed(1)} ms ± ${(best.rttMs / 2).toFixed(1)}`));
  dc.onopen = () => { logEvent('client', 'dataChannel', 'open'); S.clock.start(); };
  dc.onclose = () => logEvent('client', 'dataChannel', 'closed');
  dc.onmessage = (e) => onServerMessage(e.data);

  pc.onconnectionstatechange = () => {
    logEvent('client', 'pcState', pc.connectionState);
    setStatus(pc.connectionState + (S.sessionId ? ` · session ${S.sessionId}` : ''));
    if (pc.connectionState === 'connected' && S.qoe.connectMs === null) {
      S.qoe.connectMs = performance.now() - S.t0Perf;
    }
    if (pc.connectionState === 'failed') stop('connection failed');
  };
  pc.oniceconnectionstatechange = () => logEvent('client', 'iceState', pc.iceConnectionState);
  pc.onicegatheringstatechange = () => logEvent('client', 'iceGatheringState', pc.iceGatheringState);
  pc.onsignalingstatechange = () => logEvent('client', 'signalingState', pc.signalingState);
  pc.onicecandidateerror = (e) => logEvent('client', 'iceCandidateError', `${e.errorCode} ${e.errorText} ${e.url || ''}`);

  try {
    await pc.setLocalDescription(await pc.createOffer());
    await waitForGathering(pc, 3000);
    const res = await fetch('offer', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ sdp: pc.localDescription.sdp, type: pc.localDescription.type, mode: 'uplink', client: clientInfo() }),
    });
    const body = await res.json();
    if (!res.ok) throw new Error(body.error || res.statusText);
    S.sessionId = body.sessionId;
    S.source.label = body.sessionId.slice(0, 8);
    logEvent('client', 'answer', `session ${body.sessionId}`);
    await pc.setRemoteDescription({ type: body.type, sdp: body.sdp });
    await applySenderParams(vt.sender, cfg);
  } catch (err) {
    logEvent('client', 'error', err.message);
    stop('signaling error: ' + err.message);
    return;
  }

  $('stop').disabled = false;
  S.source.start(onSourceFrame);
  S.stopTickers.push(workerTicker(cfg.statsIntervalMs, () => { pollStats(); }));
  S.timers.push(setInterval(drawCharts, 250));
}

// Put the chosen codec first; the server answers with the first one it supports.
function preferCodec(transceiver, mimeType) {
  if (!transceiver.setCodecPreferences) {
    logEvent('client', 'warning', 'setCodecPreferences unsupported; browser picks the codec');
    return;
  }
  const caps = (RTCRtpSender.getCapabilities && RTCRtpSender.getCapabilities('video')) || RTCRtpReceiver.getCapabilities('video');
  const want = (c) => c.mimeType.toLowerCase() === mimeType.toLowerCase() &&
    (mimeType !== 'video/VP9' || !c.sdpFmtpLine || /profile-id=0/.test(c.sdpFmtpLine));
  const chosen = caps.codecs.filter(want);
  if (!chosen.length) {
    logEvent('client', 'warning', `${mimeType} not supported by this browser`);
    return;
  }
  try {
    transceiver.setCodecPreferences([...chosen, ...caps.codecs.filter((c) => !want(c))]);
  } catch (err) {
    logEvent('client', 'warning', 'setCodecPreferences: ' + err.message);
  }
}

async function applySenderParams(sender, cfg) {
  const p = sender.getParameters();
  if (!p.encodings || !p.encodings.length) p.encodings = [{}];
  if (cfg.maxBitrateKbps) p.encodings[0].maxBitrate = cfg.maxBitrateKbps * 1000;
  if (cfg.degradationPreference) p.degradationPreference = cfg.degradationPreference;
  try {
    await sender.setParameters(p);
    logEvent('client', 'senderParameters', { maxBitrate: p.encodings[0].maxBitrate, degradationPreference: p.degradationPreference });
  } catch (err) {
    logEvent('client', 'warning', 'setParameters: ' + err.message);
  }
}

function onSourceFrame(f) {
  S.frames.push(f);
  S.pendingFrames.push(f);
  const q = S.qoe;
  q.sourceFrames++;
  if (q.lastSourceAt !== null && f.perf - q.lastSourceAt > 2000 / S.source.fps) q.sourceLateTicks++;
  q.lastSourceAt = f.perf;
}

function stop(reason) {
  for (const t of S.timers) clearInterval(t);
  for (const s of S.stopTickers) s();
  S.timers = [];
  S.stopTickers = [];
  if (S.clock) S.clock.stop();
  if (S.source) S.source.stop && S.source.stop();
  if (S.pc) {
    pollStats().finally(() => {
      sendToServer({ type: 'client-bye', reason, qoe: qoeSummary() });
      setTimeout(() => {
        S.pc && S.pc.close(); S.pc = null;
        S.source && S.source.close(); S.source = null;
        S.audio && S.audio.close(); S.audio = null;
        preview.srcObject = null;
      }, 200);
    });
  }
  logEvent('client', 'stop', reason);
  setStatus('stopped: ' + reason);
  $('start').disabled = false;
  $('stop').disabled = true;
}

// ---------------------------------------------------------------------------
// getStats polling + derived metrics
// ---------------------------------------------------------------------------
async function pollStats() {
  const pc = S.pc;
  if (!pc) return;
  const report = await pc.getStats();
  const t = Date.now();
  const perf = performance.now();
  const byId = {};
  report.forEach((r) => { byId[r.id] = r; });
  const all = Object.values(byId);
  const find = (type, kind) => all.find((r) => r.type === type && (!kind || r.kind === kind));
  const vout = find('outbound-rtp', 'video');
  const aout = find('outbound-rtp', 'audio');
  const vsrc = find('media-source', 'video');
  const vri = find('remote-inbound-rtp', 'video');
  const ari = find('remote-inbound-rtp', 'audio');
  const transport = find('transport');
  let pair = transport && byId[transport.selectedCandidatePairId];
  if (!pair) pair = all.find((r) => r.type === 'candidate-pair' && (r.nominated || r.selected) && r.state === 'succeeded');
  const codec = vout && byId[vout.codecId];

  const cur = { t, vout, aout, vsrc, vri, ari, pair, codec };
  const d = derive(S.prev, cur);
  S.prev = cur;

  if (vout && vout.bytesSent > 0 && S.qoe.firstPacketSentMs === null) S.qoe.firstPacketSentMs = perf - S.t0Perf;
  const q = S.qoe;
  if (d.video.height) {
    if (q.lastHeightAt !== null) { q.heightTimeSum += q.lastHeight * (t - q.lastHeightAt); q.heightTimeDur += t - q.lastHeightAt; }
    q.lastHeight = d.video.height; q.lastHeightAt = t;
  }
  if (S.prev && S.statsSamples.length) {
    const lastReason = S.statsSamples[S.statsSamples.length - 1].derived.video.qualityLimitationReason;
    if (d.video.qualityLimitationReason && lastReason !== undefined && d.video.qualityLimitationReason !== lastReason) {
      logEvent('client', 'qualityLimitation', d.video.qualityLimitationReason);
    }
  }

  const sample = { t, perf, derived: d, raw: all };
  S.statsSamples.push(sample);

  if ($('sendRaw').checked) {
    const full = S.tick % 4 === 0;
    const dynamicTypes = new Set(['outbound-rtp', 'remote-inbound-rtp', 'media-source', 'transport', 'data-channel', 'peer-connection']);
    const raw = full ? all.filter((r) => r.type !== 'certificate')
      : all.filter((r) => dynamicTypes.has(r.type) || (pair && r.id === pair.id));
    sendToServer({
      type: 'client-stats', t, perf, tick: S.tick, full, derived: d, qoe: qoeSummary(), raw,
      frames: S.pendingFrames.splice(0), events: S.pendingEvents.splice(0),
    });
  }
  S.tick++;

  charts.bitrate.push(t, { sent: d.video.bitrateKbps, target: d.video.targetBitrateKbps, availOut: d.transport.availableOutgoingKbps });
  charts.rtt.push(t, { iceRtt: d.transport.rttMs, rtcpRtt: d.video.remoteRttMs });
  charts.fps.push(t, { source: d.source.fps, encoded: d.video.fps });
  charts.loss.push(t, { remoteLoss: d.video.remoteFractionLostPct });
  charts.delay.push(t, { encode: d.video.encodeMsPerFrame, pacer: d.video.packetSendDelayMs });
  charts.quality.push(t, { height: d.video.height, qp: d.video.qpAvg });

  $('t-derived').innerHTML = kvHTML(d);
  $('t-vout').textContent = JSON.stringify(vout, null, 2);
  $('t-aout').textContent = JSON.stringify({ audio: aout, mediaSource: vsrc, codec }, null, 2);
  $('t-pair').textContent = JSON.stringify({ remoteInboundVideo: vri, remoteInboundAudio: ari, pair, local: pair && byId[pair.localCandidateId], remote: pair && byId[pair.remoteCandidateId] }, null, 2);
  renderQoE();
}

function derive(prev, cur) {
  const dt = prev ? (cur.t - prev.t) / 1000 : undefined;
  const v = cur.vout, pv = prev && prev.vout;
  const out = { dtSec: dt, video: {}, source: {}, audio: {}, transport: {} };
  if (v) {
    const dBytes = delta(v, pv, 'bytesSent');
    const dEnc = delta(v, pv, 'framesEncoded');
    const dSent = delta(v, pv, 'framesSent');
    const dPkts = delta(v, pv, 'packetsSent');
    const dEncTime = delta(v, pv, 'totalEncodeTime');
    const dQp = delta(v, pv, 'qpSum');
    const dSendDelay = delta(v, pv, 'totalPacketSendDelay');
    const dRetx = delta(v, pv, 'retransmittedBytesSent');
    Object.assign(out.video, {
      codec: cur.codec && cur.codec.mimeType,
      bitrateKbps: dt ? dBytes * 8 / dt / 1000 : undefined,
      targetBitrateKbps: v.targetBitrate !== undefined ? v.targetBitrate / 1000 : undefined,
      retransmitKbps: dt && dRetx !== undefined ? dRetx * 8 / dt / 1000 : undefined,
      fps: v.framesPerSecond ?? (dt && dEnc !== undefined ? dEnc / dt : undefined),
      sentFps: dt && dSent !== undefined ? dSent / dt : undefined,
      width: v.frameWidth, height: v.frameHeight,
      encodeMsPerFrame: dEnc > 0 && dEncTime !== undefined ? 1000 * dEncTime / dEnc : undefined,
      qpAvg: dEnc > 0 && dQp !== undefined ? dQp / dEnc : undefined,
      packetSendDelayMs: dPkts > 0 && dSendDelay !== undefined ? 1000 * dSendDelay / dPkts : undefined,
      qualityLimitationReason: v.qualityLimitationReason,
      qualityLimitationDurations: v.qualityLimitationDurations,
      qualityLimitationResolutionChanges: v.qualityLimitationResolutionChanges,
      keyFramesEncodedDelta: delta(v, pv, 'keyFramesEncoded'),
      hugeFramesSent: v.hugeFramesSent,
      nackDelta: delta(v, pv, 'nackCount'), pliDelta: delta(v, pv, 'pliCount'), firDelta: delta(v, pv, 'firCount'),
      nackCount: v.nackCount, pliCount: v.pliCount, firCount: v.firCount,
      framesEncoded: v.framesEncoded, framesSent: v.framesSent, keyFramesEncoded: v.keyFramesEncoded,
      packetsSent: v.packetsSent, bytesSent: v.bytesSent, retransmittedPacketsSent: v.retransmittedPacketsSent,
      encoderImplementation: v.encoderImplementation, powerEfficientEncoder: v.powerEfficientEncoder,
      scalabilityMode: v.scalabilityMode,
    });
  }
  const r = cur.vri;
  if (r) {
    Object.assign(out.video, {
      remoteRttMs: r.roundTripTime !== undefined ? r.roundTripTime * 1000 : undefined,
      remoteFractionLostPct: r.fractionLost !== undefined ? 100 * r.fractionLost : undefined,
      remoteJitterMs: r.jitter !== undefined ? r.jitter * 1000 : undefined,
      remotePacketsLost: r.packetsLost,
    });
  }
  const src = cur.vsrc;
  if (src) {
    out.source = {
      fps: src.framesPerSecond ?? (dt && delta(src, prev && prev.vsrc, 'frames') !== undefined ? delta(src, prev.vsrc, 'frames') / dt : undefined),
      width: src.width, height: src.height, frames: src.frames,
    };
  }
  const a = cur.aout;
  if (a) {
    const ar = cur.ari;
    out.audio = {
      bitrateKbps: dt ? delta(a, prev && prev.aout, 'bytesSent') * 8 / dt / 1000 : undefined,
      packetsSent: a.packetsSent,
      remoteRttMs: ar && ar.roundTripTime !== undefined ? ar.roundTripTime * 1000 : undefined,
      remoteFractionLostPct: ar && ar.fractionLost !== undefined ? 100 * ar.fractionLost : undefined,
      remoteJitterMs: ar && ar.jitter !== undefined ? ar.jitter * 1000 : undefined,
      remotePacketsLost: ar && ar.packetsLost,
    };
  }
  const p = cur.pair;
  if (p) {
    out.transport = {
      rttMs: p.currentRoundTripTime !== undefined ? p.currentRoundTripTime * 1000 : undefined,
      availableOutgoingKbps: p.availableOutgoingBitrate !== undefined ? p.availableOutgoingBitrate / 1000 : undefined,
      bytesSent: p.bytesSent,
      sendKbps: dt && prev && prev.pair ? (p.bytesSent - prev.pair.bytesSent) * 8 / dt / 1000 : undefined,
    };
  }
  return out;
}

// ---------------------------------------------------------------------------
// QoE summary (cumulative). Receive-side numbers come from the server.
// ---------------------------------------------------------------------------
function meanOf(vals) {
  const v = vals.filter(Number.isFinite);
  return v.length ? v.reduce((x, y) => x + y, 0) / v.length : undefined;
}
function pctOf(vals, p) {
  const v = vals.filter(Number.isFinite).sort((x, y) => x - y);
  return v.length ? v[Math.floor(p * (v.length - 1))] : undefined;
}

function qoeSummary() {
  const q = S.qoe;
  if (!q) return null;
  const last = S.statsSamples[S.statsSamples.length - 1];
  const v = last && last.derived.video;
  const first = S.statsSamples.find((s) => s.derived.video.bytesSent > 0);
  const sendSec = first && last ? (last.t - first.t) / 1000 : 0;
  const sv = (lastServer() || {}).uplink?.video || {};
  const up = S.serverSnaps.map((m) => (m.uplink || {}).video || {});
  return {
    connectMs: q.connectMs, firstPacketSentMs: q.firstPacketSentMs, serverFirstFrameMs: q.serverFirstFrameMs,
    sendSec,
    codec: v && v.codec, encoder: v && v.encoderImplementation,
    sourceFrames: q.sourceFrames, sourceLateTicks: q.sourceLateTicks,
    framesEncoded: v && v.framesEncoded, framesSent: v && v.framesSent,
    avgSentKbps: v && first && sendSec > 0 ? (v.bytesSent - first.derived.video.bytesSent) * 8 / sendSec / 1000 : undefined,
    meanTargetKbps: meanOf(S.statsSamples.map((s) => s.derived.video.targetBitrateKbps)),
    meanAvailableOutgoingKbps: meanOf(S.statsSamples.map((s) => s.derived.transport.availableOutgoingKbps)),
    meanEncodeFps: meanOf(S.statsSamples.map((s) => s.derived.video.fps)),
    meanEncodeMs: meanOf(S.statsSamples.map((s) => s.derived.video.encodeMsPerFrame)),
    meanQp: meanOf(S.statsSamples.map((s) => s.derived.video.qpAvg)),
    timeWeightedHeight: q.heightTimeDur > 0 ? q.heightTimeSum / q.heightTimeDur : undefined,
    resolutionChanges: v && v.qualityLimitationResolutionChanges,
    qualityLimitationDurations: v && v.qualityLimitationDurations,
    nackCount: v && v.nackCount, pliCount: v && v.pliCount, firCount: v && v.firCount,
    meanRttMs: meanOf(S.statsSamples.map((s) => s.derived.transport.rttMs)),
    // Server (receiver) side
    serverFramesReceived: sv.framesReceived, serverFramesLost: sv.framesLost,
    serverMeanFps: meanOf(up.map((u) => u.fps)),
    serverFreezeCount: sv.freezeCount, serverTotalFreezesMs: sv.totalFreezesMs, serverFreezeRatio: sv.freezeRatio,
    serverLongestFreezeMs: sv.longestFreezeMs,
    serverPacketsLost: sv.packetsLost, serverPacketsExpected: sv.packetsExpected,
    serverLossPct: sv.packetsExpected ? 100 * sv.packetsLost / sv.packetsExpected : undefined,
    serverPlisSent: (lastServer() || {}).uplink?.plisSent,
    meanOwdMs: meanOf(up.map((u) => u.owdMsMean)),
    p95OwdMs: pctOf(up.map((u) => u.owdMsP95), 0.95),
    meanOwdWallMs: meanOf(up.map((u) => u.owdWallMsMean)),
    meanDelayVarMs: meanOf(up.map((u) => u.delayVarMsMean)),
    senderNtpMinusWallMs: sv.senderReport?.ntpMinusWallClockMs,
    clockOffsetMs: S.clock && S.clock.best ? S.clock.best.offsetMs : undefined,
    clockRttMs: S.clock && S.clock.best ? S.clock.best.rttMs : undefined,
  };
}

function renderQoE() {
  const s = qoeSummary();
  if (!s || !S.statsSamples.length) return;
  const d = S.statsSamples[S.statsSamples.length - 1].derived;
  const sv = (lastServer() || {}).uplink?.video || {};
  $('qoe').innerHTML = tilesHTML([
    ['Server 1st frame', s.serverFirstFrameMs, 'ms'],
    ['Codec', s.codec ? `${s.codec.replace('video/', '')}` : null, s.encoder || ''],
    ['Sent resolution', d.video.width ? `${d.video.width}×${d.video.height}` : null, ''],
    ['Encode FPS', d.video.fps, ''],
    ['Server FPS', sv.fps, ''],
    ['Quality limit', d.video.qualityLimitationReason || null, ''],
    ['Sent bitrate', d.video.bitrateKbps, 'kbps'],
    ['Encoder target', d.video.targetBitrateKbps, 'kbps'],
    ['Avail. outgoing', d.transport.availableOutgoingKbps, 'kbps'],
    ['One-way delay', sv.owdMsMean, 'ms'],
    ['Delay variation', sv.delayVarMsMean, 'ms'],
    ['Server freezes', s.serverFreezeCount, s.serverTotalFreezesMs !== undefined ? `${fmtNum(s.serverTotalFreezesMs / 1000)} s` : ''],
    ['Freeze ratio', s.serverFreezeRatio !== undefined ? 100 * s.serverFreezeRatio : undefined, '%'],
    ['Packet loss', s.serverLossPct, '%'],
    ['Frames lost', s.serverFramesLost, ''],
    ['PLIs', s.serverPlisSent, ''],
    ['Clock offset', s.clockOffsetMs, s.clockRttMs !== undefined ? `ms ±${fmtNum(s.clockRttMs / 2)}` : 'ms'],
  ]);
}

function lastServer() { return S.serverSnaps[S.serverSnaps.length - 1]; }

// ---------------------------------------------------------------------------
// Server telemetry
// ---------------------------------------------------------------------------
function onServerMessage(data) {
  let msg;
  try { msg = JSON.parse(data); } catch { return; }
  const recvT = Date.now();
  switch (msg.type) {
    case 'clock-pong':
      S.clock && S.clock.onPong(msg);
      if (S.clock && S.clock.best) charts.rtt.push(recvT, { clockRtt: S.clock.best.rttMs });
      break;
    case 'server-hello':
      S.serverHello = { ...msg, clientRecvT: recvT };
      logEvent('server', 'hello', `uplink session on ${msg.host.hostname}`);
      break;
    case 'server-event':
      logEvent('server', msg.event.kind, msg.event.value, { serverT: msg.event.t });
      if (msg.event.kind === 'uplink' && msg.event.value === 'first video frame' && S.qoe.serverFirstFrameMs === null) {
        S.qoe.serverFirstFrameMs = performance.now() - S.t0Perf;
      }
      break;
    case 'server-stats': {
      msg.clientRecvT = recvT;
      S.serverSnaps.push(msg);
      const u = msg.uplink || {};
      const v = u.video || {};
      charts.bitrate.push(recvT, { serverRecv: v.bitrate !== undefined ? v.bitrate / 1000 : undefined });
      charts.fps.push(recvT, { serverRecv: v.fps });
      const fl = v.windowFramesLost, fr = v.windowFrames;
      charts.loss.push(recvT, { serverLoss: v.windowLossPct, serverFrameLoss: fl + fr > 0 ? 100 * fl / (fl + fr) : undefined });
      charts.delay.push(recvT, { owd: v.owdMsMean, owdWall: v.owdWallMsMean, delayVar: v.delayVarMsMean, assembly: v.assemblyMsMean });
      const view = { video: v, audio: u.audio, plisSent: u.plisSent, clockOffsetMs: u.clockOffsetMs, codecs: { video: u.videoCodec, audio: u.audioCodec }, rtcpRecv: u.rtcpRecv, state: msg.state, selectedCandidatePair: msg.selectedCandidatePair };
      $('t-server').innerHTML = kvHTML(view);
      $('t-server-raw').textContent = JSON.stringify(msg, null, 2);
      break;
    }
  }
}

function sendToServer(obj) { sendOnChannel(S.dc, obj); }

// ---------------------------------------------------------------------------
// Export
// ---------------------------------------------------------------------------
function download() {
  downloadJSON(`puffer-webrtc-uplink_${S.sessionId || 'session'}_${new Date(S.t0Wall).toISOString().replace(/[:.]/g, '-')}.json`, {
    mode: 'uplink', sessionId: S.sessionId, startedAt: S.t0Wall, client: clientInfo(), qoe: qoeSummary(),
    serverHello: S.serverHello, events: S.events, stats: S.statsSamples, frames: S.frames, server: S.serverSnaps,
  });
}
