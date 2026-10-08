'use strict';

// ---------------------------------------------------------------------------
// Session state. Everything collected is kept here and can be downloaded.
// ---------------------------------------------------------------------------
const S = {
  pc: null,
  dc: null,
  sessionId: null,
  t0Perf: 0,            // performance.now() at Start click
  t0Wall: 0,            // Date.now() at Start click
  timers: [],
  statsSamples: [],     // {t, perf, raw: [...reports], derived}
  frames: [],           // requestVideoFrameCallback metadata
  events: [],           // {t, src, kind, value}
  serverSnaps: [],      // server-stats messages (with client receive time)
  serverHello: null,
  prev: null,           // previous stats map for deltas
  qoe: null,
  pendingFrames: [],    // not yet sent to server
  pendingEvents: [],
  tick: 0,
};

const video = $('video');

const charts = {
  bitrate: new Chart($('c-bitrate'), [
    { key: 'recvVideo', name: 'client video recv' },
    { key: 'ccTarget', name: 'server CC target' },
    { key: 'encOut', name: 'server encoder out' },
    { key: 'encCfg', name: 'encoder configured' },
    { key: 'availIn', name: 'client avail. incoming' },
  ]),
  rtt: new Chart($('c-rtt'), [
    { key: 'clientRtt', name: 'client ICE RTT' },
    { key: 'serverRtt', name: 'server RTCP RTT' },
  ]),
  fps: new Chart($('c-fps'), [
    { key: 'decoded', name: 'decoded' },
    { key: 'rendered', name: 'rendered (rVFC)' },
    { key: 'encoded', name: 'server encoded' },
  ]),
  loss: new Chart($('c-loss'), [
    { key: 'videoLoss', name: 'client video loss' },
    { key: 'serverLoss', name: 'server RR fraction lost' },
    { key: 'gccLoss', name: 'GCC avg loss' },
    { key: 'audioConceal', name: 'audio concealed' },
  ]),
  delay: new Chart($('c-delay'), [
    { key: 'jb', name: 'jitter buffer' },
    { key: 'decode', name: 'decode/frame' },
    { key: 'e2e', name: 'est. end-to-end' },
    { key: 'gccDelay', name: 'GCC delay estimate' },
  ]),
  quality: new Chart($('c-quality'), [
    { key: 'height', name: 'frame height (px)' },
    { key: 'qp', name: 'avg QP' },
  ]),
};

function drawCharts() {
  const now = Date.now();
  for (const c of Object.values(charts)) c.draw(now);
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------
const logEvent = makeEventLogger(S, $('events'));

for (const name of ['loadedmetadata', 'loadeddata', 'canplay', 'playing', 'waiting', 'stalled',
  'pause', 'play', 'ended', 'error', 'resize', 'suspend', 'emptied']) {
  video.addEventListener(name, () => {
    const v = name === 'resize' ? `${video.videoWidth}x${video.videoHeight}` : video.currentTime.toFixed(3);
    logEvent('client', 'video.' + name, v);
    if (!S.qoe) return;
    if (name === 'waiting') S.qoe.waitingEvents++;
    // rVFC doesn't fire in background tabs, so also take startup from the first decoded frame.
    if (name === 'loadeddata' && S.qoe.startupMs === null) {
      S.qoe.startupMs = performance.now() - S.t0Perf;
      logEvent('client', 'firstFrame', `${S.qoe.startupMs.toFixed(0)} ms after start (loadeddata)`);
    }
  });
}
document.addEventListener('visibilitychange', () => {
  logEvent('client', 'visibility', document.visibilityState);
  if (S.qoe) {
    // Don't count hidden time as a stall or against render fps.
    S.qoe.lastFrameAt = null;
    S.qoe.renderFpsCount = 0;
    S.qoe.lastRenderFpsAt = performance.now();
  }
});

// ---------------------------------------------------------------------------
// Start / stop
// ---------------------------------------------------------------------------
$('start').onclick = start;
$('stop').onclick = () => stop('user stop');
$('download').onclick = download;

// Rate control settings: the sine inputs only apply to the sine algorithm.
const rcInputs = ['rcAlgo', 'sineCenter', 'sineAmp', 'sinePeriod'];
function updateRateControlUI() {
  const sine = $('rcAlgo').value === 'sine';
  for (const el of document.querySelectorAll('.sine-opt')) el.hidden = !sine;
}
$('rcAlgo').onchange = updateRateControlUI;
updateRateControlUI();

function rateControlRequest() {
  if ($('rcAlgo').value !== 'sine') return undefined;
  return {
    algorithm: 'sine',
    centerKbps: Number($('sineCenter').value),
    amplitudeKbps: Number($('sineAmp').value),
    periodSec: Number($('sinePeriod').value),
  };
}

function setStatus(s) { $('status').textContent = s; }

function newQoE() {
  return {
    startupMs: null,           // Start click -> first rendered frame
    connectMs: null,           // Start click -> pc connected
    firstPacketMs: null,       // Start click -> first video bytes
    renderedFrames: 0,
    renderStalls: 0,           // gaps between rendered frames > threshold
    renderStallMs: 0,
    longestStallMs: 0,
    lastFrameAt: null,
    lastPresented: null,
    presentedGaps: 0,          // frames the compositor skipped
    waitingEvents: 0,
    resolutionChanges: 0,
    lastRes: null,
    heightTimeSum: 0,          // for time-weighted resolution
    heightTimeDur: 0,
    lastRenderFpsAt: performance.now(),
    renderFpsCount: 0,
    renderFps: null,
  };
}

async function start() {
  $('start').disabled = true;
  $('download').disabled = false;
  for (const id of rcInputs) $(id).disabled = true;
  Object.assign(S, {
    statsSamples: [], frames: [], events: [], serverSnaps: [], serverHello: null, prev: null,
    pendingFrames: [], pendingEvents: [], tick: 0, qoe: newQoE(),
  });
  $('events').innerHTML = '';
  for (const c of Object.values(charts)) for (const s of c.series) s.pts = [];
  S.t0Perf = performance.now();
  S.t0Wall = Date.now();
  logEvent('client', 'start', new Date(S.t0Wall).toISOString());
  setStatus('connecting…');

  const stun = $('stun').value.trim();
  const pc = new RTCPeerConnection({ iceServers: stun ? [{ urls: stun }] : [] });
  S.pc = pc;
  pc.addTransceiver('video', { direction: 'recvonly' });
  pc.addTransceiver('audio', { direction: 'recvonly' });
  const dc = pc.createDataChannel('telemetry', { ordered: true });
  S.dc = dc;
  dc.onopen = () => logEvent('client', 'dataChannel', 'open');
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

  pc.ontrack = (e) => {
    logEvent('client', 'track', e.track.kind);
    const jb = $('jbTarget').value;
    if (jb !== '' && 'jitterBufferTarget' in e.receiver) {
      e.receiver.jitterBufferTarget = Number(jb);
      logEvent('client', 'jitterBufferTarget', Number(jb));
    }
    if (video.srcObject !== e.streams[0]) {
      video.srcObject = e.streams[0];
      video.play().catch((err) => logEvent('client', 'playError', err.message));
    }
  };

  try {
    await pc.setLocalDescription(await pc.createOffer());
    await waitForGathering(pc, 3000);
    const res = await fetch('offer', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        sdp: pc.localDescription.sdp, type: pc.localDescription.type, client: clientInfo(),
        rateControl: rateControlRequest(),
      }),
    });
    const body = await res.json();
    if (!res.ok) throw new Error(body.error || res.statusText);
    S.sessionId = body.sessionId;
    logEvent('client', 'answer', `session ${body.sessionId}`);
    await pc.setRemoteDescription({ type: body.type, sdp: body.sdp });
  } catch (err) {
    logEvent('client', 'error', err.message);
    stop('signaling error: ' + err.message);
    return;
  }

  $('stop').disabled = false;
  if ('requestVideoFrameCallback' in HTMLVideoElement.prototype) {
    video.requestVideoFrameCallback(onFrame);
  } else {
    logEvent('client', 'warning', 'requestVideoFrameCallback unsupported; render-stall metrics disabled');
  }
  const interval = Math.max(50, Number($('interval').value) || 250);
  S.timers.push(setInterval(pollStats, interval));
  S.timers.push(setInterval(drawCharts, 250));
}

function stop(reason) {
  for (const t of S.timers) clearInterval(t);
  S.timers = [];
  if (S.pc) {
    // Flush a final sample to the server log before closing.
    pollStats().finally(() => {
      sendToServer({ type: 'client-bye', reason, qoe: qoeSummary() });
      setTimeout(() => { S.pc && S.pc.close(); S.pc = null; }, 200);
    });
  }
  logEvent('client', 'stop', reason);
  setStatus('stopped: ' + reason);
  $('start').disabled = false;
  $('stop').disabled = true;
  for (const id of rcInputs) $(id).disabled = false;
}

function clientInfo() {
  return {
    ...deviceInfo(),
    startedAtWall: S.t0Wall,
    statsIntervalMs: Number($('interval').value),
    jitterBufferTargetMs: $('jbTarget').value === '' ? null : Number($('jbTarget').value),
  };
}

// ---------------------------------------------------------------------------
// Per-frame metrics via requestVideoFrameCallback
// ---------------------------------------------------------------------------
function onFrame(now, meta) {
  const q = S.qoe;
  if (!q || !S.pc) return;
  const rec = {
    now, t: Date.now(),
    mediaTime: meta.mediaTime, presentedFrames: meta.presentedFrames,
    expectedDisplayTime: meta.expectedDisplayTime, presentationTime: meta.presentationTime,
    width: meta.width, height: meta.height,
    processingDuration: meta.processingDuration, receiveTime: meta.receiveTime,
    captureTime: meta.captureTime, rtpTimestamp: meta.rtpTimestamp,
  };
  S.frames.push(rec);
  S.pendingFrames.push(rec);

  if (q.startupMs === null) {
    q.startupMs = now - S.t0Perf;
    logEvent('client', 'firstFrame', `${q.startupMs.toFixed(0)} ms after start (rVFC)`);
  }
  q.renderedFrames++;
  q.renderFpsCount++;
  if (now - q.lastRenderFpsAt >= 1000) {
    q.renderFps = q.renderFpsCount * 1000 / (now - q.lastRenderFpsAt);
    q.renderFpsCount = 0;
    q.lastRenderFpsAt = now;
  }
  if (q.lastPresented !== null && meta.presentedFrames - q.lastPresented > 1) {
    q.presentedGaps += meta.presentedFrames - q.lastPresented - 1;
  }
  q.lastPresented = meta.presentedFrames;

  // Render stall: gap between displayed frames well beyond the frame interval.
  const fps = (S.prev && S.prev.derivedFps) || 30;
  const threshold = Math.max(150, 3 * 1000 / fps);
  if (q.lastFrameAt !== null && !document.hidden) {
    const gap = now - q.lastFrameAt;
    if (gap > threshold) {
      q.renderStalls++;
      q.renderStallMs += gap;
      q.longestStallMs = Math.max(q.longestStallMs, gap);
      logEvent('client', 'renderStall', `${gap.toFixed(0)} ms`, { gapMs: gap });
    }
    if (meta.height) { q.heightTimeSum += meta.height * gap; q.heightTimeDur += gap; }
  }
  q.lastFrameAt = now;
  const res = `${meta.width}x${meta.height}`;
  if (q.lastRes && q.lastRes !== res) q.resolutionChanges++;
  q.lastRes = res;

  video.requestVideoFrameCallback(onFrame);
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
  const vin = find('inbound-rtp', 'video');
  const ain = find('inbound-rtp', 'audio');
  const transport = find('transport');
  let pair = transport && byId[transport.selectedCandidatePairId];
  if (!pair) pair = all.find((r) => r.type === 'candidate-pair' && (r.nominated || r.selected) && r.state === 'succeeded');
  const localCand = pair && byId[pair.localCandidateId];
  const remoteCand = pair && byId[pair.remoteCandidateId];
  const playout = find('media-playout');

  const cur = { t, vin, ain, pair, playout };
  const d = derive(S.prev, cur);
  cur.derivedFps = d.video.fps;
  S.prev = cur;

  if (vin && vin.bytesReceived > 0 && S.qoe.firstPacketMs === null) {
    S.qoe.firstPacketMs = perf - S.t0Perf;
  }

  const sample = { t, perf, derived: d, raw: all };
  S.statsSamples.push(sample);

  // Ship to server log. Full report every 4th tick, the time-varying parts otherwise.
  if ($('sendRaw').checked) {
    const full = S.tick % 4 === 0;
    const dynamicTypes = new Set(['inbound-rtp', 'remote-outbound-rtp', 'transport', 'media-playout', 'track', 'data-channel', 'peer-connection']);
    const raw = full ? all.filter((r) => r.type !== 'certificate')
      : all.filter((r) => dynamicTypes.has(r.type) || (pair && r.id === pair.id));
    sendToServer({
      type: 'client-stats', t, perf, tick: S.tick, full, derived: d, qoe: qoeSummary(), raw,
      frames: S.pendingFrames.splice(0), events: S.pendingEvents.splice(0),
    });
  }
  S.tick++;

  // Charts
  charts.bitrate.push(t, { recvVideo: d.video.bitrateKbps, availIn: d.transport.availableIncomingKbps });
  charts.rtt.push(t, { clientRtt: d.transport.rttMs });
  charts.fps.push(t, { decoded: d.video.fps, rendered: S.qoe.renderFps });
  charts.loss.push(t, { videoLoss: d.video.lossPct, audioConceal: d.audio.concealedPct });
  charts.delay.push(t, { jb: d.video.jitterBufferMs, decode: d.video.decodeMsPerFrame, e2e: d.video.estimatedE2EMs });
  charts.quality.push(t, { height: vin && vin.frameHeight, qp: d.video.qpAvg });

  renderTables(d, vin, ain, { pair, localCand, remoteCand, transport, playout });
  renderQoE();
}

function derive(prev, cur) {
  const dt = prev ? (cur.t - prev.t) / 1000 : undefined;
  const v = cur.vin, pv = prev && prev.vin;
  const a = cur.ain, pa = prev && prev.ain;
  const out = { dtSec: dt, video: {}, audio: {}, transport: {} };
  if (v) {
    const dBytes = delta(v, pv, 'bytesReceived');
    const dDecoded = delta(v, pv, 'framesDecoded');
    const dLost = delta(v, pv, 'packetsLost');
    const dRecv = delta(v, pv, 'packetsReceived');
    const dJbDelay = delta(v, pv, 'jitterBufferDelay');
    const dJbCount = delta(v, pv, 'jitterBufferEmittedCount');
    const dDecode = delta(v, pv, 'totalDecodeTime');
    const dProc = delta(v, pv, 'totalProcessingDelay');
    const dQp = delta(v, pv, 'qpSum');
    const dAssembly = delta(v, pv, 'totalAssemblyTime');
    const dInter = delta(v, pv, 'totalInterFrameDelay');
    const dInterSq = delta(v, pv, 'totalSquaredInterFrameDelay');
    Object.assign(out.video, {
      bitrateKbps: dt ? dBytes * 8 / dt / 1000 : undefined,
      fps: v.framesPerSecond ?? (dt && dDecoded !== undefined ? dDecoded / dt : undefined),
      width: v.frameWidth, height: v.frameHeight,
      lossPct: dLost !== undefined && dRecv !== undefined && dLost + dRecv > 0 ? 100 * dLost / (dLost + dRecv) : undefined,
      jitterMs: v.jitter !== undefined ? v.jitter * 1000 : undefined,
      jitterBufferMs: dJbCount > 0 ? 1000 * dJbDelay / dJbCount : undefined,
      jitterBufferTargetMs: dJbCount > 0 && delta(v, pv, 'jitterBufferTargetDelay') !== undefined ? 1000 * delta(v, pv, 'jitterBufferTargetDelay') / dJbCount : undefined,
      jitterBufferMinimumMs: dJbCount > 0 && delta(v, pv, 'jitterBufferMinimumDelay') !== undefined ? 1000 * delta(v, pv, 'jitterBufferMinimumDelay') / dJbCount : undefined,
      decodeMsPerFrame: dDecoded > 0 ? 1000 * dDecode / dDecoded : undefined,
      processingMsPerFrame: dDecoded > 0 && dProc !== undefined ? 1000 * dProc / dDecoded : undefined,
      assemblyMsPerFrame: dAssembly !== undefined && delta(v, pv, 'framesAssembledFromMultiplePackets') > 0 ? 1000 * dAssembly / delta(v, pv, 'framesAssembledFromMultiplePackets') : undefined,
      qpAvg: dDecoded > 0 && dQp !== undefined ? dQp / dDecoded : undefined,
      interFrameMsMean: dDecoded > 0 && dInter !== undefined ? 1000 * dInter / dDecoded : undefined,
      interFrameMsStd: dDecoded > 1 && dInterSq !== undefined ? 1000 * Math.sqrt(Math.max(0, dInterSq / dDecoded - (dInter / dDecoded) ** 2)) : undefined,
      framesDroppedDelta: delta(v, pv, 'framesDropped'),
      keyFramesDecodedDelta: delta(v, pv, 'keyFramesDecoded'),
      nackDelta: delta(v, pv, 'nackCount'),
      pliDelta: delta(v, pv, 'pliCount'),
      firDelta: delta(v, pv, 'firCount'),
      freezeCount: v.freezeCount, totalFreezesDurationSec: v.totalFreezesDuration,
      pauseCount: v.pauseCount, totalPausesDurationSec: v.totalPausesDuration,
      framesDecoded: v.framesDecoded, framesDropped: v.framesDropped, framesReceived: v.framesReceived,
      packetsLost: v.packetsLost, packetsReceived: v.packetsReceived,
      nackCount: v.nackCount, pliCount: v.pliCount, firCount: v.firCount,
      decoderImplementation: v.decoderImplementation, powerEfficientDecoder: v.powerEfficientDecoder,
      cumulativeJitterBufferMs: v.jitterBufferEmittedCount ? 1000 * v.jitterBufferDelay / v.jitterBufferEmittedCount : undefined,
    });
  }
  if (a) {
    const dConc = delta(a, pa, 'concealedSamples');
    const dTot = delta(a, pa, 'totalSamplesReceived');
    const dBytes = delta(a, pa, 'bytesReceived');
    const dJbDelay = delta(a, pa, 'jitterBufferDelay');
    const dJbCount = delta(a, pa, 'jitterBufferEmittedCount');
    Object.assign(out.audio, {
      bitrateKbps: dt ? dBytes * 8 / dt / 1000 : undefined,
      concealedPct: dTot > 0 ? 100 * dConc / dTot : undefined,
      silentConcealedDelta: delta(a, pa, 'silentConcealedSamples'),
      concealmentEventsDelta: delta(a, pa, 'concealmentEvents'),
      insertedForDecelerationDelta: delta(a, pa, 'insertedSamplesForDeceleration'),
      removedForAccelerationDelta: delta(a, pa, 'removedSamplesForAcceleration'),
      jitterBufferMs: dJbCount > 0 ? 1000 * dJbDelay / dJbCount : undefined,
      jitterMs: a.jitter !== undefined ? a.jitter * 1000 : undefined,
      audioLevel: a.audioLevel,
      packetsLost: a.packetsLost,
      concealedSamples: a.concealedSamples, totalSamplesReceived: a.totalSamplesReceived,
      cumulativeConcealedPct: a.totalSamplesReceived ? 100 * a.concealedSamples / a.totalSamplesReceived : undefined,
    });
  }
  const p = cur.pair;
  if (p) {
    out.transport = {
      rttMs: p.currentRoundTripTime !== undefined ? p.currentRoundTripTime * 1000 : undefined,
      availableIncomingKbps: p.availableIncomingBitrate !== undefined ? p.availableIncomingBitrate / 1000 : undefined,
      bytesReceived: p.bytesReceived,
      recvKbps: dt && prev && prev.pair ? (p.bytesReceived - prev.pair.bytesReceived) * 8 / dt / 1000 : undefined,
    };
  }
  // Rough glass-to-glass-ish estimate: network one-way (RTT/2) + jitter buffer + decode.
  if (out.video.jitterBufferMs !== undefined && out.transport.rttMs !== undefined) {
    out.video.estimatedE2EMs = out.transport.rttMs / 2 + out.video.jitterBufferMs + (out.video.decodeMsPerFrame || 0);
  }
  return out;
}

// ---------------------------------------------------------------------------
// QoE summary (cumulative for the session)
// ---------------------------------------------------------------------------
function qoeSummary() {
  const q = S.qoe;
  if (!q) return null;
  const last = S.statsSamples[S.statsSamples.length - 1];
  const v = last && last.derived.video;
  const a = last && last.derived.audio;
  const first = S.statsSamples.find((s) => s.derived.video.framesDecoded > 0);
  const playSec = first && last ? (last.t - first.t) / 1000 : 0;
  const vin = S.prev && S.prev.vin;
  const fpsVals = S.statsSamples.map((s) => s.derived.video.fps).filter(Number.isFinite);
  const meanFps = fpsVals.length ? fpsVals.reduce((x, y) => x + y, 0) / fpsVals.length : undefined;
  const rttVals = S.statsSamples.map((s) => s.derived.transport.rttMs).filter(Number.isFinite);
  return {
    startupMs: q.startupMs, connectMs: q.connectMs, firstPacketMs: q.firstPacketMs,
    playSec,
    renderedFrames: q.renderedFrames,
    renderStalls: q.renderStalls, renderStallMs: q.renderStallMs, longestStallMs: q.longestStallMs,
    renderStallRatio: playSec > 0 ? q.renderStallMs / 1000 / playSec : undefined,
    presentedGaps: q.presentedGaps,
    waitingEvents: q.waitingEvents,
    freezeCount: v && v.freezeCount, totalFreezesSec: v && v.totalFreezesDurationSec,
    freezeRatio: v && playSec > 0 && v.totalFreezesDurationSec !== undefined ? v.totalFreezesDurationSec / playSec : undefined,
    pauseCount: v && v.pauseCount,
    avgVideoKbps: vin && playSec > 0 && first ? (vin.bytesReceived - (first.raw.find((r) => r.type === 'inbound-rtp' && r.kind === 'video')?.bytesReceived || 0)) * 8 / playSec / 1000 : undefined,
    meanFps,
    fpsStd: fpsVals.length > 1 ? Math.sqrt(fpsVals.reduce((s, x) => s + (x - meanFps) ** 2, 0) / fpsVals.length) : undefined,
    timeWeightedHeight: q.heightTimeDur > 0 ? q.heightTimeSum / q.heightTimeDur : undefined,
    resolutionChanges: q.resolutionChanges,
    framesDropped: v && v.framesDropped,
    packetsLost: v && v.packetsLost, packetsReceived: v && v.packetsReceived,
    lossPct: v && v.packetsReceived ? 100 * v.packetsLost / (v.packetsLost + v.packetsReceived) : undefined,
    nackCount: v && v.nackCount, pliCount: v && v.pliCount, firCount: v && v.firCount,
    meanJitterBufferMs: v && v.cumulativeJitterBufferMs,
    meanRttMs: rttVals.length ? rttVals.reduce((x, y) => x + y, 0) / rttVals.length : undefined,
    audioConcealedPct: a && a.cumulativeConcealedPct,
    decoder: v && v.decoderImplementation,
  };
}

function renderQoE() {
  const s = qoeSummary();
  if (!s) return;
  const last = S.statsSamples[S.statsSamples.length - 1].derived;
  const tiles = [
    ['Startup', s.startupMs, 'ms'],
    ['Video bitrate', last.video.bitrateKbps, 'kbps'],
    ['Resolution', last.video.width ? `${last.video.width}×${last.video.height}` : null, ''],
    ['FPS (render)', S.qoe.renderFps, ''],
    ['RTT', last.transport.rttMs, 'ms'],
    ['Jitter buffer', last.video.jitterBufferMs, 'ms'],
    ['Freezes', s.freezeCount, s.totalFreezesSec !== undefined ? `${fmtNum(s.totalFreezesSec)} s` : ''],
    ['Render stalls', s.renderStalls, `${fmtNum(s.renderStallMs / 1000)} s`],
    ['Stall ratio', s.renderStallRatio !== undefined ? 100 * s.renderStallRatio : undefined, '%'],
    ['Packet loss', s.lossPct, '%'],
    ['Frames dropped', s.framesDropped, ''],
    ['Audio concealed', s.audioConcealedPct, '%'],
    ['Avg video', s.avgVideoKbps, 'kbps'],
    ['CC target', (lastServer()?.rateControl ?? lastServer()?.gcc)?.targetBitrate / 1000, 'kbps'],
  ];
  $('qoe').innerHTML = tilesHTML(tiles);
}

function lastServer() { return S.serverSnaps[S.serverSnaps.length - 1]; }

function renderTables(d, vin, ain, extra) {
  $('t-derived').innerHTML = kvHTML(d);
  $('t-vin').textContent = JSON.stringify(vin, null, 2);
  $('t-ain').textContent = JSON.stringify(ain, null, 2);
  $('t-pair').textContent = JSON.stringify(extra, null, 2);
}

// ---------------------------------------------------------------------------
// Server telemetry
// ---------------------------------------------------------------------------
function onServerMessage(data) {
  let msg;
  try { msg = JSON.parse(data); } catch { return; }
  const recvT = Date.now();
  switch (msg.type) {
    case 'server-hello':
      S.serverHello = { ...msg, clientRecvT: recvT };
      logEvent('server', 'hello', `${msg.media.width}x${msg.media.height}@${fmtNum(msg.media.fps)} from ${msg.host.hostname}`);
      if (msg.rateControl) logEvent('server', 'rateControl', describeRateControl(msg.rateControl));
      break;
    case 'server-event':
      logEvent('server', msg.event.kind, msg.event.value, { serverT: msg.event.t });
      break;
    case 'server-stats': {
      msg.clientRecvT = recvT;
      S.serverSnaps.push(msg);
      const g = msg.gcc || {}, rc = msg.rateControl || {}, e = msg.encoder || {}, v = (msg.interceptorStats || {}).video || {};
      const ccTarget = rc.targetBitrate !== undefined ? rc.targetBitrate : g.targetBitrate;
      charts.bitrate.push(recvT, { ccTarget: ccTarget / 1000, encOut: e.outputBitrate / 1000, encCfg: e.configuredBitrate / 1000 });
      const rrRtt = ((msg.rtcp || {}).video || {}).rttMs;
      charts.rtt.push(recvT, { serverRtt: v.rttMs > 0 ? v.rttMs : rrRtt });
      charts.fps.push(recvT, { encoded: e.outputFps });
      charts.loss.push(recvT, { serverLoss: v.fractionLost !== undefined ? 100 * v.fractionLost : undefined, gccLoss: g.averageLoss !== undefined ? 100 * g.averageLoss : undefined });
      charts.delay.push(recvT, { gccDelay: g.delayEstimate });
      const view = { rateControl: msg.rateControl, gcc: msg.gcc, encoder: e, media: msg.media, rtcp: msg.rtcp, rtp: { video: v.outbound, audio: ((msg.interceptorStats || {}).audio || {}).outbound }, state: msg.state, selectedCandidatePair: msg.selectedCandidatePair };
      $('t-server').innerHTML = kvHTML(view);
      $('t-server-raw').textContent = JSON.stringify(msg, null, 2);
      break;
    }
  }
}

function describeRateControl(rc) {
  if (rc.algorithm !== 'sine') return rc.algorithm;
  return `sine ${rc.centerBps / 1000} ± ${rc.amplitudeBps / 1000} kbps, period ${rc.periodSec} s`;
}

function sendToServer(obj) { sendOnChannel(S.dc, obj); }

// ---------------------------------------------------------------------------
// Export
// ---------------------------------------------------------------------------
function download() {
  downloadJSON(`puffer-webrtc_${S.sessionId || 'session'}_${new Date(S.t0Wall).toISOString().replace(/[:.]/g, '-')}.json`, {
    sessionId: S.sessionId, startedAt: S.t0Wall, client: clientInfo(), qoe: qoeSummary(),
    serverHello: S.serverHello, events: S.events, stats: S.statsSamples, frames: S.frames, server: S.serverSnaps,
  });
}
