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

## Running on a server (Docker)

```sh
docker compose up --build      # Linux host; uses host networking
```

Open TCP **8080** and UDP **50000** in the firewall, then browse to `http://<server>:8080`.

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
| `--max-sessions` | 8 | |

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

## Notes and caveats for experiments
- **Telemetry uses the same path as the media.** It goes over SCTP, which GCC does not account for. Server to client is about 10 KB/s at the defaults. Client to server (upstream) is larger. To reduce it, raise `--telemetry-interval` or `--full-stats-every`, or untick "Send client stats" in the page settings.
- **Pion's GCC starts conservatively.** Right after the first feedback it often cuts the target to about 250 kbps, then ramps up by roughly 8% per second. With simple content, x264 also undershoots its target, so the sender is application-limited and the ramp is slower still. Adjust `--start-bitrate`/`--min-bitrate`, or compare against your own real content.
- **Background tabs:** browsers pause `requestVideoFrameCallback` in background tabs. Hidden periods are left out of stall accounting, and each visibility change is logged.
- **Firefox** exposes fewer inbound-rtp fields (for example, no `freezeCount`). Missing fields show as `–`.
- **Network emulation:** Chrome DevTools throttling does not affect WebRTC. On Linux, use `tc qdisc add dev <if> root netem rate 2mbit delay 40ms loss 1%`; on macOS, use Network Link Conditioner.
