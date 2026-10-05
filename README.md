# puffer-webrtc

Streams a pre-recorded video (audio and video) to browsers over WebRTC. It records the client's QoE and WebRTC statistics together with the server's view: the congestion controller, the encoder and RTCP feedback.

```
mp4 ─ffmpeg (decode, real time, loop)─┬─ raw I420 ─► x264 (cgo) ─► H.264 track ─┐
                                      └─ Opus/Ogg ─────────────────► Opus track ─┤─► Pion ─► browser
GCC (TWCC feedback) ── target bitrate ──► x264_encoder_reconfig                  │
RTCP PLI/FIR ─► force IDR     server telemetry ◄── DataChannel "telemetry" ──────┘
```

- **Live encoding per viewer:** the encoder bitrate follows Pion's send-side Google Congestion Control (GCC), which works like a real WebRTC sender. With `--headroom 0.9`, the video bitrate is set to `0.9 × GCC target − audio bitrate`.
- **Codecs:** H.264 constrained-baseline video, no B-frames, and IDR frames only on PLI/FIR by default (`--keyint` changes this). Audio is Opus, 20 ms frames, 64 kbps.
- **Signaling:** a single `POST /offer` without trickle ICE. Media uses one UDP port (default 50000).
- **Uplink test:** `/uplink.html` reverses the direction. The browser sends a generated video and a tone, and the server receives and measures them. See [Uplink test](#uplink-test).

## Running on a server (Docker)

```sh
docker compose up --build      # Linux host; uses host networking
```

Open TCP **8080** and UDP **50000** in the firewall, then browse to `http://<server>:8080`.

- **macOS (Docker Desktop):** host networking only reaches Docker's Linux VM, so use the override that publishes the ports and advertises `127.0.0.1`: `docker compose -f docker-compose.yml -f docker-compose.mac.yml up --build` (or `make docker-mac`). Set `PUFFER_HOST_HTTP_PORT` if 8080 is taken on the Mac.
- **Cloud VMs:** if the public IP is not on a network interface (AWS, GCP), set `PUFFER_PUBLIC_IP=<public ip>` in `docker-compose.yml`. The server then puts that IP in its ICE candidates.
- **Your own video:** mount the file and set `PUFFER_VIDEO` (see the commented lines in `docker-compose.yml`). The server probes resolution and frame rate at startup. Any container or codec that ffmpeg can decode works.
- **HTTPS:** a receive-only page works over plain http. For HTTPS, pass `--tls-cert` and `--tls-key`, or put the server behind a reverse proxy. Only `/offer` and the static files go through HTTP; media always goes to the UDP port directly.
- **CPU cost:** each viewer uses one ffmpeg decode and one x264 encode, about one core at 720p30 with the `veryfast` preset. `--max-sessions` (default 8) sets the cap.

## Local development (macOS)

```sh
brew install go x264 pkg-config    # ffmpeg must also be on PATH
make run                           # builds, generates media/standin.mp4, serves :8080
```

## Flags

Every flag can also be set as an environment variable: `--foo-bar` becomes `PUFFER_FOO_BAR`.

| flag | default | |
|---|---|---|
| `--video` | `media/standin.mp4` | source file, looped forever |
| `--http-addr` | `:8080` | |
| `--udp-port` | `50000` | single UDP port for all media (0 = ephemeral) |
| `--public-ip` | | IPs to advertise instead of the interface IPs (1:1 NAT) |
| `--stun` | | STUN URL(s), comma-separated |
| `--start-bitrate` / `--min-bitrate` / `--max-bitrate` | 1M / 100k / 6M | GCC limits (bps) |
| `--audio-bitrate` | 64000 | Opus bitrate |
| `--headroom` | 0.9 | share of the GCC target given to the payload |
| `--keyint` | 0 | seconds between periodic IDRs (0 = only on request) |
| `--x264-preset` / `--x264-threads` | veryfast / 0 | |
| `--telemetry-interval` | 250ms | how often server stats are sent and logged |
| `--full-stats-every` | 4 | also include the full Pion `GetStats()` every N ticks |
| `--log-dir` | `logs` | per-session JSONL logs |
| `--max-sessions` | 8 | shared by downlink and uplink sessions |
| `--uplink-record` | false | save received uplink media (`.h264`/`.ivf`/`.ogg`) next to each session log |

## What gets measured

### Client (browser)
- **`getStats()`**, polled every 250 ms by default. The full report is stored; the fields that matter most for QoE are:
  - **Video:**
    - Freezes and pauses: `freezeCount`/`totalFreezesDuration`, `pauseCount`.
    - Frames: `framesDropped`, `framesDecoded`/`framesReceived`, `framesPerSecond`, frame size.
    - Delay: `jitterBufferDelay`, plus its target and minimum, `totalDecodeTime`, `totalProcessingDelay`, `totalAssemblyTime`, and inter-frame delay mean and standard deviation.
    - `qpSum`.
    - Feedback and loss: NACK/PLI/FIR counts, loss, jitter.
    - Decoder: `decoderImplementation`.
  - **Audio:**
    - Concealment: concealed and silent samples, concealment events.
    - Time-stretching: inserted and removed samples.
    - The audio jitter buffer and `media-playout`.
  - **Transport:** RTT and available incoming bitrate from the selected candidate pair.
- **`requestVideoFrameCallback`:** logs every rendered frame with presentation time, `presentedFrames`, `processingDuration`, `receiveTime` and `rtpTimestamp`. From these the client computes:
  - **Render stalls:** gaps longer than max(150 ms, 3 frame intervals).
  - **Render fps.**
  - **Skipped presentations.**
- **`<video>` element events** (waiting, stalled, resize and so on) and connection state changes.
- **QoE summary:**
  - Startup delay.
  - Stall count, duration and ratio.
  - Freeze ratio.
  - Average bitrate.
  - Mean fps and its standard deviation.
  - Time-weighted resolution and resolution changes.
  - Loss.
  - Audio concealment percentage.
  - Mean jitter buffer delay and RTT.

### Server (streamed to the page and logged)
- **GCC:**
  - Target bitrate.
  - Loss-based and delay-based targets, average loss.
  - Delay measurement, estimate and threshold.
  - Usage (over/normal/under) and state.
- **Encoder:**
  - Output bitrate and fps.
  - Configured vs GCC target bitrate.
  - Frame sizes.
  - Keyframes: total and PLI-forced counts, and time since the last one.
  - Encode time (mean, p95, max).
  - Time spent in `WriteSample`, which includes pacer queueing.
- **RTCP received from the browser:**
  - PLI/FIR/NACK counts, including the number of NACKed sequence numbers.
  - TWCC feedback count.
  - REMB.
  - The last receiver report (fraction lost, total lost, jitter), and RTT computed from LSR/DLSR.
- **Pion stats interceptor:** outbound packets and bytes, plus remote-inbound data (loss, jitter, RTT).
- **Full Pion `GetStats()`:** candidate pairs, transports and the data channel.
- **Other:**
  - Connection states and the selected candidate pair.
  - Position in the source file and loop count.
  - Host info (CPU count, ffmpeg and x264 versions, Pion module versions).

### Logs
Each session writes `logs/<UTC time>_<session>.jsonl`. Every line is `{"t": <server unix ms>, "src": ..., "kind": ..., "data": ...}`, where `src` is one of:
- `meta`: session setup, including the SDP offer and answer, client info and config.
- `event`: server-side state changes.
- `server`: telemetry snapshots.
- `client`: the browser's `getStats()` output.

The browser sends its stats over the DataChannel. Every 4th tick it sends the full report; on the other ticks it sends only the parts that change.

The **Download JSON** button saves everything the client collected, including every rendered-frame record.

## Uplink test

Open `http://<server>:8080/uplink.html` (or use the **Downlink | Uplink** switch in the header) and press Start. The page sends `mode: "uplink"` to the same `/offer` endpoint.

```
canvas (hidden, worker-timed) ─► browser encoder (H264/VP8/VP9) ─┐
WebAudio tone + 1 kHz beep/s ──► Opus ───────────────────────────┤─► Pion (receive only) ─► frame reassembly + stats
browser GCC ◄── TWCC feedback, RR, NACK, PLI ◄────────────────────┘          └─► optional .h264/.ivf/.ogg recording
```

- **Source:** a 1280×720@30 canvas that is never added to the page. It shows scrolling colour bars, a moving box, a noise patch (so the encoder always has work), and an overlay with the frame number, wall-clock time and session id. A Web Worker drives the frame timer, so the frame rate holds in background tabs. **Show source preview** in Settings displays it for debugging.
- **Settings:**
  - Codec (H264 by default, VP8, VP9), applied with `setCodecPreferences`.
  - Resolution and frame rate.
  - Max bitrate and `degradationPreference`, applied with `setParameters`.
  - `contentHint`.
  - Audio on/off.
- **The server sends feedback but never decodes.** It sends TWCC feedback, so the browser's own congestion control sets the bitrate, plus receiver reports and NACKs. It sends a PLI (at most every 500 ms) after it loses a frame.

### What gets measured (uplink)
- **Client, from `getStats()`:**
  - `outbound-rtp`: sent bitrate and `targetBitrate`; encoded and sent fps; sent resolution, which shows encoder downscaling; encode time per frame; QP; `qualityLimitationReason` and its durations; pacer delay per packet (`totalPacketSendDelay`); retransmissions; NACK, PLI and FIR received; encoder implementation.
  - `media-source`: the capture frame rate.
  - `remote-inbound-rtp`: RTT, loss and jitter, from the server's RRs.
  - Candidate pair: `availableOutgoingBitrate` (the browser's GCC estimate).
- **Server, from frames reassembled out of the RTP packets:**
  - Received bitrate and fps.
  - Keyframes (detected per codec); frames lost (still incomplete 300 ms after a newer frame completed, or undecodable until the next keyframe).
  - Packet loss, duplicates and reordering.
  - Frame assembly time; inter-frame gap mean and std.
  - Freezes (gap > max(3 × average gap, average gap + 150 ms), the WebRTC definition), plus freeze ratio.
  - Time to first frame; PLIs sent; the browser's SRs; resolution, for VP8 and VP9 keyframes.
  - Audio: packets, loss and jitter.
- **Delay:** each frame's RTP timestamp is mapped to the browser's capture time through its RTCP Sender Reports.
  - `owdMs*` is the main one-way delay, from capture in the browser to the frame's last packet reaching the server. It is anchored to the SRs: `d − min(SR arrival − SR NTP time) + RTT/2`. So it includes encode and pacer time, assumes the minimum path delay is symmetric, and does not need the browser's clock to match the server's.
  - `owdWallMs*` uses the DataChannel clock sync (`server wall clock − Date.now()`, best of 10 s of pings) instead. Chrome's RTCP NTP clock is not exactly `Date.now()`; it was about 14 ms off in testing, and `senderReport.ntpMinusWallClockMs` reports the difference. Treat this value as a cross-check.
  - `delayVarMs*` is `d − min(d)`. It needs no clock assumptions and shows queueing build-up directly.
  - Audio delays are measured the same way and include the 20 ms Opus frame duration.
- **Logs:** `logs/<UTC time>_<session>_uplink.jsonl`, in the same record format as the downlink. Server snapshots have an `uplink` section instead of `gcc`/`encoder`. Client `client-stats` carry the outbound stats and the per-frame source log (frame number and generation time). `--uplink-record` writes the received media next to the log; under loss the recording may contain broken frames, since packets are written in arrival order.

## Notes and caveats for experiments
- **Telemetry uses the same path as the media.** It goes over SCTP, which GCC does not account for. Server to client is about 10 KB/s at the defaults. Client to server (upstream) is larger. To reduce it, raise `--telemetry-interval` or `--full-stats-every`, or untick "Send client stats" in the page settings.
- **Pion's GCC starts conservatively.** Right after the first feedback it often cuts the target to about 250 kbps, then ramps up by roughly 8% per second. With simple content, x264 also undershoots its target, so the sender is application-limited and the ramp is slower still. Adjust `--start-bitrate`/`--min-bitrate`, or compare against your own real content.
- **Background tabs:** browsers pause `requestVideoFrameCallback` in background tabs. Hidden periods are left out of stall accounting, and each visibility change is logged.
- **Firefox** exposes fewer inbound-rtp fields (for example, no `freezeCount`). Missing fields show as `–`.
- **Network emulation:** Chrome DevTools throttling does not affect WebRTC. On Linux, use `tc qdisc add dev <if> root netem rate 2mbit delay 40ms loss 1%`; on macOS, use Network Link Conditioner.
