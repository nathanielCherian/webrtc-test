'use strict';

// ---------------------------------------------------------------------------
// One-way delay over WebTransport, against owdserver (owdserver/ in the repo).
// The server sends a datagram every interval stamped with its send time; we
// ping it over the same session to learn the clock offset, NTP-style, keeping
// the lowest-RTT sample of the last 10 s (offset error at most rtt/2, and any
// path asymmetry shows up as a constant bias, not as changes in the delay).
//
// Wire format, big-endian; server times are Unix microseconds:
//   beacon  [0]=1 [1:5] seq u32 [5:13] tSend i64 [13:] padding
//   ping    [0]=2 [1:5] id u32  [5:13] t1 f64 (our ms)
//   pong    [0]=3 [1:5] id u32  [5:13] t1 f64 [13:21] t2 i64 (received) [21:29] t3 i64 (sent)
// ---------------------------------------------------------------------------
class OwdProbe {
  // onBeacon({t, seq, owdMs}) and onPong({t, rttMs, upMs, offsetMs}) are called
  // with t = Date.now() for charting; owdMs/upMs are undefined until the first pong.
  constructor(url, { certHash, intervalMs, size, onBeacon, onPong, onSync, onClose } = {}) {
    this.url = new URL(url);
    if (intervalMs) this.url.searchParams.set('interval', intervalMs);
    if (size) this.url.searchParams.set('size', size);
    this.certHash = certHash;
    this.cb = { onBeacon, onPong, onSync, onClose };
    this.beacons = [];  // {seq, tSend, tRecv} in ms; tSend on the server clock
    this.pings = [];    // {id, t1, t2, t3, t4, rttMs, offsetMs}
    this.recent = [];   // pings of the last 10 s
    this.syncs = [];    // best-sample changes: {at, offsetMs, rttMs}
    this.best = null;
    this.firstSeq = null;
    this.maxSeq = -1;
    this.reordered = 0;
    this.nextId = 1;
    this.timer = null;
    this.wt = null;
  }

  // Wall-clock ms at sub-ms resolution. Drifts from Date.now() over time, but
  // the offset is re-estimated every few seconds from this same clock.
  static now() { return performance.timeOrigin + performance.now(); }

  async hashes() {
    if (this.certHash) return [this.certHash];
    // owdserver publishes its self-signed certificate's hash on the same port
    // over plain HTTP. An https page may not be allowed to fetch it (mixed
    // content); then paste the hash into the settings instead.
    try {
      const res = await fetch(`http://${this.url.host}/cert-hash`, { cache: 'no-store' });
      return (await res.json()).hashes || [];
    } catch {
      return []; // e.g. a CA-issued certificate behind https only
    }
  }

  async start() {
    if (typeof WebTransport === 'undefined') {
      throw new Error(window.isSecureContext ? 'this browser has no WebTransport'
        : 'WebTransport needs a secure context: open the page over https or via localhost');
    }
    const hashes = await this.hashes();
    const opts = hashes.length ? {
      serverCertificateHashes: hashes.map((h) => ({ algorithm: 'sha-256', value: Uint8Array.from(atob(h), (c) => c.charCodeAt(0)) })),
    } : {};
    this.wt = new WebTransport(this.url.href, opts);
    this.wt.closed.then(() => this.cb.onClose?.('closed'), (err) => this.cb.onClose?.(err.message));
    await this.wt.ready;
    this.writer = this.wt.datagrams.writable.getWriter();
    this.ping();
    this.timer = setInterval(() => this.ping(), 250);
    this.readLoop();
  }

  close() {
    clearInterval(this.timer);
    this.timer = null;
    try { this.wt && this.wt.close(); } catch { /* already closed */ }
  }

  ping() {
    const b = new DataView(new ArrayBuffer(13));
    b.setUint8(0, 2);
    b.setUint32(1, this.nextId++);
    b.setFloat64(5, OwdProbe.now());
    this.writer.write(new Uint8Array(b.buffer)).catch(() => {});
  }

  async readLoop() {
    const reader = this.wt.datagrams.readable.getReader();
    for (;;) {
      let r;
      try { r = await reader.read(); } catch { return; }
      if (r.done) return;
      const tRecv = OwdProbe.now();
      const d = new DataView(r.value.buffer, r.value.byteOffset, r.value.byteLength);
      if (d.byteLength >= 13 && d.getUint8(0) === 1) this.onBeacon(d, tRecv);
      else if (d.byteLength >= 29 && d.getUint8(0) === 3) this.onPongMsg(d, tRecv);
    }
  }

  onBeacon(d, tRecv) {
    const seq = d.getUint32(1);
    const tSend = Number(d.getBigInt64(5)) / 1000;
    this.beacons.push({ seq, tSend, tRecv });
    if (this.firstSeq === null) this.firstSeq = seq;
    if (seq < this.maxSeq) this.reordered++;
    this.maxSeq = Math.max(this.maxSeq, seq);
    const owdMs = this.best ? tRecv + this.best.offsetMs - tSend : undefined;
    this.cb.onBeacon?.({ t: Date.now(), seq, owdMs });
  }

  onPongMsg(d, t4) {
    const id = d.getUint32(1);
    const t1 = d.getFloat64(5);
    const t2 = Number(d.getBigInt64(13)) / 1000;
    const t3 = Number(d.getBigInt64(21)) / 1000;
    // Server minus client clock; rtt excludes the server's turnaround time.
    const s = { id, t1, t2, t3, t4, rttMs: (t4 - t1) - (t3 - t2), offsetMs: ((t2 - t1) + (t3 - t4)) / 2 };
    this.pings.push(s);
    this.recent.push(s);
    while (t4 - this.recent[0].t4 > 10000) this.recent.shift();
    const best = this.recent.reduce((a, b) => (b.rttMs < a.rttMs ? b : a));
    if (best !== this.best) {
      this.best = best;
      this.syncs.push({ at: t4, offsetMs: best.offsetMs, rttMs: best.rttMs });
      this.cb.onSync?.(best);
    }
    this.cb.onPong?.({ t: Date.now(), rttMs: s.rttMs, upMs: t2 - (t1 + this.best.offsetMs), offsetMs: this.best.offsetMs });
  }

  summary() {
    const n = this.beacons.length;
    const expected = n ? this.maxSeq - this.firstSeq + 1 : 0;
    const owd = this.best ? this.beacons.slice(-50).map((b) => b.tRecv + this.best.offsetMs - b.tSend) : [];
    return {
      url: this.url.href,
      beaconsReceived: n,
      beaconsLost: Math.max(0, expected - n),
      lossPct: expected ? 100 * Math.max(0, expected - n) / expected : undefined,
      reordered: this.reordered,
      clockOffsetMs: this.best && this.best.offsetMs,
      clockRttMs: this.best && this.best.rttMs,
      owdLastMs: owd.length ? owd[owd.length - 1] : undefined,
      owdMinRecentMs: owd.length ? Math.min(...owd) : undefined,
      owdMeanRecentMs: owd.length ? owd.reduce((a, b) => a + b, 0) / owd.length : undefined,
    };
  }

  // Everything needed to recompute the delays offline.
  export() {
    return { url: this.url.href, summary: this.summary(), beacons: this.beacons, pings: this.pings, syncs: this.syncs };
  }
}
