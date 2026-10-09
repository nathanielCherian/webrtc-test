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
- **Rate control:** GCC by default. The page's Settings can switch a session to an open-loop sine or square wave instead. See [Rate control](#rate-control).
- **Shared simulcast source:** with `--simulcast`, viewers can instead receive pre-encoded renditions (for example from the [sfu-test](#shared-simulcast-source) transcoder). Each viewer keeps its own PeerConnection and rate control, but nothing is encoded per viewer: the rate control picks which rendition to forward. See [Shared simulcast source](#shared-simulcast-source).
- **Uplink test:** `/uplink.html` reverses the direction. The browser sends a generated video and a tone, and the server receives and measures them. See [Uplink test](#uplink-test).

## Running on a server (Docker)

```sh
docker compose up --build      # Linux host; uses host networking
```

Open TCP **8080** and UDP **50000** in the firewall, then browse to `http://<server>:8080`.

- **macOS (Docker Desktop):** host networking only reaches Docker's Linux VM, so use the override that publishes the ports and advertises `127.0.0.1`: `docker compose -f docker-compose.yml -f docker-compose.mac.yml up --build` (or `make docker-mac`). Set `PUFFER_HOST_HTTP_PORT` if 8080 is taken on the Mac.
- **Cloud VMs:** if the public IP is not on a network interface (AWS, GCP), set `PUFFER_PUBLIC_IP=<public ip>` in `docker-compose.yml`. The server then puts that IP in its ICE candidates.
- **Your own video:** pick or upload one on the page (see [Choosing the video](#choosing-the-video)). With Compose, the library is `./videos` on the host, so you can also copy files there. To change the *default* video, mount the file and set `PUFFER_VIDEO` (see the commented lines in `docker-compose.yml`).
- **HTTPS:** a receive-only page works over plain http. For HTTPS, pass `--tls-cert` and `--tls-key`, or put the server behind a reverse proxy. Only `/offer` and the static files go through HTTP; media always goes to the UDP port directly.
- **CPU cost:** each viewer uses one ffmpeg decode and one x264 encode, about one core at 720p30 with the `veryfast` preset. `--max-sessions` (default 8) sets the cap.

## Watching from a machine that can only reach the server over SSH

If the server is not reachable from your computer except through SSH (for example a lab machine with no public
internet), WebRTC can still use UDP at both ends: `cmd/udptunnel` carries the UDP datagrams over an SSH port forward
and keeps their boundaries. The browser does ordinary WebRTC over UDP to a relay on its own machine, and the server
receives ordinary UDP from a relay next to it.

```
browser ──UDP──► udptunnel local ──TCP──► ssh -L ──TCP──► udptunnel remote ──UDP──► server :50000
(your machine, 127.0.0.1:50000)                            (server, 127.0.0.1:50001)
```

```sh
# on the server
./bin/server --public-ip 127.0.0.1 ...            # ICE candidates point at the browser's local relay
./bin/udptunnel remote -tcp 127.0.0.1:50001 -udp 127.0.0.1:50000

# on your machine
ssh -L 8080:localhost:8080 -L 50001:localhost:50001 <server>
udptunnel local -udp 127.0.0.1:50000 -tcp 127.0.0.1:50001
# then open http://localhost:8080
```

Build the local relay for your machine with `GOOS=darwin GOARCH=arm64 go build -o udptunnel ./cmd/udptunnel`
(or `windows`/`linux`, `amd64`). The relays log packet counts every 10 s.

The hop between the relays is TCP, so loss and delay on it are retransmitted and in order instead of real UDP loss.
GCC and QoE numbers therefore reflect the SSH path, not a normal network path. It is fine for watching the stream and
checking layer switches. To emulate network conditions, use `tc netem` on the server's loopback or Network Link
Conditioner on your machine.

`--udp-bind` binds the media port to one IP. With a loopback IP, loopback candidates are enabled, which is
mainly useful for testing the tunnel on a single machine.

## Local development (macOS)

```sh
brew install go x264 pkg-config    # ffmpeg must also be on PATH
make run                           # builds, generates media/standin.mp4, serves :8080
```

## Flags

Every flag can also be set as an environment variable: `--foo-bar` becomes `PUFFER_FOO_BAR`.

| flag | default | |
|---|---|---|
| `--video` | `media/standin.mp4` | default source file, looped forever |
| `--media-dir` | the `--video` file's directory | videos the page can choose from and upload to |
| `--allow-upload` | true | allow uploads from the page to `--media-dir` |
| `--max-upload-mb` | 2048 | largest accepted upload |
| `--http-addr` | `:8080` | |
| `--udp-port` | `50000` | single UDP port for all media (0 = ephemeral) |
| `--public-ip` | | IPs to advertise instead of the interface IPs (1:1 NAT; `127.0.0.1` for the SSH/UDP tunnel) |
| `--udp-bind` | all interfaces | IP address the media port binds to |
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
| `--simulcast` | | renditions for the shared simulcast source: `name=host:port@bitrate,...` (MPEG-TS over UDP), e.g. `high=127.0.0.1:5001@2500k,mid=127.0.0.1:5002@800k,low=127.0.0.1:5003@250k` |
| `--simulcast-audio` | highest layer | rendition whose AAC audio is transcoded (once) to Opus; `none` for no audio |

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
- **Rate control** (`rateControl`): the algorithm and its current target bitrate. In sine and square modes it also has the wave parameters (and `duty` for square), the elapsed time, the phase (0–1 within the current cycle) and the number of TWCC/RTCP feedback packets received.
- **GCC** (GCC sessions only):
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

## Choosing the video

**Settings → Video** on the downlink page lists the `--video` default first, then every file in `--media-dir` that ffmpeg can play, with its resolution, frame rate and length. Each session can use a different video, and the server probes each file when you first select it (results are cached). **Upload** adds a file from your computer to `--media-dir`. The server checks that ffmpeg can play it, rejects names that already exist, and selects it once the upload finishes. Uploads can be turned off with `--allow-upload=false`.

The chosen file is recorded in the session's `meta` record and `server-hello` (`media.path`). The API is `GET /videos` for the listing, `POST /videos?name=<file>` with the raw file as the body to upload, and `"video": "<file>"` in the `/offer` body (omit it for the default).

Anyone who can open the page can upload. On a public server, set `--allow-upload=false` or put it behind a reverse proxy with authentication.

## Rate control

Under **Settings → Rate control** on the downlink page, choose either:
- **GCC (default):** the normal send-side congestion control described above.
- **Sine wave (no GCC):** the send rate is a fixed function of time, `rate(t) = center + amplitude · sin(2πt / period)`. t = 0 is when media starts, so the wave starts at the center and rises first. GCC does not run, and the rate ignores network feedback.
- **Square wave (no GCC):** the rate alternates between high = `center + amplitude` and low = `center − amplitude`. Each period starts high, and the duty cycle (default 50%) sets the share of the period spent high. Because amplitude is less than the center, the low phase never drops to zero.

In both wave modes the rate is the total send rate, just like a GCC target. It goes to Pion's leaky-bucket pacer (the same one GCC uses), and the encoder gets `headroom × rate − audio bitrate`. The session is still a normal WebRTC session: TWCC header extensions, TWCC feedback, NACK retransmissions, PLI/FIR keyframes and RTCP SR/RR all work as before, and the TWCC feedback is only counted. The server checks the parameters: amplitude must be less than the center, the period must be at least 1 s, the duty cycle must be strictly between 0% and 100%, and the peak (center + amplitude) can't exceed `--max-bitrate`.

The choice is per session and goes in the `/offer` body, e.g. `"rateControl": {"algorithm": "sine", "centerKbps": 1000, "amplitudeKbps": 500, "periodSec": 10}`, or `"algorithm": "square"` with an optional `"dutyPct": 30`. It is recorded in the session's `meta` record, in `server-hello` and in each snapshot's `rateControl` section. Uplink sessions ignore it.

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

## Shared simulcast source

The default source encodes the video once per viewer. The simulcast source encodes it once for everyone: an external encoder produces a few constant-bitrate H.264 renditions, the server ingests them, and every viewer forwards one of them.

```
                    MPEG-TS/UDP                  ┌─ viewer A: PeerConnection + GCC ──► forwards high ─► browser
transcoder ─┬─ high 2500k :5001 ─┐               │
(sfu-test)  ├─ mid   800k :5002 ─┼─► hub ────────┼─ viewer B: PeerConnection + GCC ──► forwards mid ──► browser
            └─ low   250k :5003 ─┘ (demux once,  │
                                   AAC→Opus once)└─ viewer C: PeerConnection + square wave ─► low/mid ─► browser
```

- **Hub (`internal/simulcast`):** listens on each rendition's UDP port, demultiplexes the MPEG-TS, and reads each layer's resolution from its SPS. It shares every access unit with all viewers without copying. The AAC audio of one rendition goes through a single shared ffmpeg to become Opus.
- **Per viewer (`internal/session/simulcast.go`):** the viewer's own bandwidth estimator gives a video budget (`headroom × target − audio`, as before). The forwarded layer is the highest one whose nominal bitrate fits:
  - **Down:** switches as soon as the current layer no longer fits.
  - **Up:** switches once the budget has covered 1.1× the higher layer for 1 s.
  - **When:** a switch happens at the target layer's next IDR. This relies on the renditions having IDRs at the same timestamps (the sfu-test transcoder guarantees this). All layers share one 90 kHz clock, so the RTP timestamps stay continuous across a switch.
  - **Packetisation:** each viewer packetises the frames itself on a `TrackLocalStaticRTP`, so the sequence numbers and timestamps are its own.
- **Probing:** Pion's GCC never raises its estimate above 1.5× the rate it sees acknowledged, so a viewer sending a fixed-rate low layer would never discover that a higher layer fits. As libwebrtc does, a GCC viewer below the top layer pads its send rate up to its estimate (at most what the next layer needs). The padding is sent as RTP padding-only packets, which browsers discard. Pion's GCC only counts payload bytes, so probes go through GCC as zero-filled payloads and become padding just before SRTP (`ratecontrol.PaddingProbes`). Padding pauses for 3 s after GCC lowers its target. Wave modes never pad, because their target is open-loop.
- **Keyframes:** the encoder is shared, so a viewer's PLI/FIR can't force an IDR. The viewer instead stops sending until the next periodic IDR of its layer, and the event is counted as `keyframeWaits`. The GOP length of the renditions therefore bounds three things: startup time, how quickly a down-switch takes effect, and recovery after a PLI. With 2 s GOPs, a down-switch can keep sending the old layer for up to 2 s.

### Running it

```sh
# 1. sfu-test: mock live feed + transcoder (sends the renditions to UDP 5001-5003)
~/sfu-test/source/serve_source.sh &
(cd ~/sfu-test && .venv/bin/python -m transcoder) &

# 2. this server, with the renditions as the simulcast source
./bin/server --simulcast high=127.0.0.1:5001@2500k,mid=127.0.0.1:5002@800k,low=127.0.0.1:5003@250k
```

On the page, **Settings → Video** lists "Live simulcast (shared encoder)" first and selects it by default. The bitrate chart then shows:
- the forwarded layer's nominal bitrate (under "encoder configured / simulcast layer"),
- the probe padding,
- the client's received rate.

Layer switches appear in **Events**. The server table has a `simulcast` section with:
- the current layer, any pending switch and the budget,
- the switch count, keyframe waits and padding,
- frames sent per layer.

The `media` section shows the hub's per-layer status: measured bitrate, resolution, fps, keyframes and TS continuity errors.

The API is `"source": "simulcast"` in the `/offer` body. `GET /info` and `GET /videos` include the hub status under `simulcast`.

The UDP ports are bound by this server. Anything else that reads them, such as sfu-test's `tools/relay.sh`, must use different ports. The renditions are 720p60 at level 3.2, while the SDP signals level 3.1 (`42e01f`). Browsers decode them anyway; strict decoders may not.

### Headless test client

`cmd/simclient` is a receive-only Pion viewer for testing without a browser. It negotiates like the page, sends real TWCC/RTCP feedback, and prints:
- the server's target, layer and padding for each viewer,
- what each viewer received.

It can also record viewer 0's H.264.

```sh
go build -o bin/simclient ./cmd/simclient
./bin/simclient -n 3 -duration 60s                                     # 3 GCC viewers
./bin/simclient -rc square -center 1500 -amp 1200 -period 16           # force layer switches
./bin/simclient -record c0.h264 && ffprobe -show_frames c0.h264        # check the forwarded stream decodes
```

## Notes and caveats for experiments
- **Telemetry uses the same path as the media.** It goes over SCTP, which GCC does not account for. Server to client is about 10 KB/s at the defaults. Client to server (upstream) is larger. To reduce it, raise `--telemetry-interval` or `--full-stats-every`, or untick "Send client stats" in the page settings.
- **Pion's GCC starts conservatively.** Right after the first feedback it often cuts the target to about 250 kbps, then ramps up by roughly 8% per second. With simple content, x264 also undershoots its target, so the sender is application-limited and the ramp is slower still. Adjust `--start-bitrate`/`--min-bitrate`, or compare against your own real content.
- **Background tabs:** browsers pause `requestVideoFrameCallback` in background tabs. Hidden periods are left out of stall accounting, and each visibility change is logged.
- **Firefox** exposes fewer inbound-rtp fields (for example, no `freezeCount`). Missing fields show as `–`.
- **Network emulation:** Chrome DevTools throttling does not affect WebRTC. On Linux, use `tc qdisc add dev <if> root netem rate 2mbit delay 40ms loss 1%`; on macOS, use Network Link Conditioner.
