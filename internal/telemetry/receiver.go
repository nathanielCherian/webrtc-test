package telemetry

import (
	"math"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// Receive-side measurements for the uplink test. Frames are reassembled from
// RTP packets (without decoding) to get frame rate, freezes and per-frame
// delay.
//
// Delay: each frame's RTP timestamp is mapped to the sender's NTP clock with
// the latest RTCP Sender Report, giving its capture time; d = arrival -
// capture then mixes the network delay with the offset between the server's
// clock and the browser's NTP clock. Chrome's NTP clock is not exactly the
// system wall clock (it was ~15 ms off in testing), so the offset is
// estimated from the SRs themselves: SRs are not paced, so
// min(SR arrival - SR NTP time) is the clock offset plus the minimum one-way
// delay, taken as RTT/2. Hence
//
//	owd     = d - min(SR arrival - SR NTP) + RTT/2       (anchored to the SRs)
//	owdWall = d - (server wall - browser Date.now())      (DataChannel clock sync; only right if Chrome's NTP == Date.now)
//	delayVar = d - min(d)                                 (no clock assumptions)

const (
	lostFrameAfter  = 300 * time.Millisecond // incomplete this long after a newer frame completed = lost
	staleFrameAfter = 2 * time.Second
	gapHistory      = 30 // frames used for the freeze threshold's average inter-frame gap
)

// Clock is what the session knows about the browser's clock, from the
// DataChannel ping/pong. Fields are NaN when unknown.
type Clock struct {
	OffsetMs float64 // server wall clock minus browser Date.now()
	RTTMs    float64 // round-trip time of the best ping
}

// srMapping relates the sender's RTP clock to its NTP clock.
type srMapping struct {
	ntpMs     float64 // sender NTP clock, unix ms
	rtpTs     uint32
	at        time.Time
	packets   uint32
	octets    uint32
	received  bool
	minSRMs   float64 // min(SR arrival - SR NTP time)
	srSamples int64
}

func (m *srMapping) observe(sr *rtcp.SenderReport) {
	now := time.Now()
	m.ntpMs = ntpToUnixMs(sr.NTPTime)
	if d := msOf(now) - m.ntpMs; m.srSamples == 0 || d < m.minSRMs {
		m.minSRMs = d
	}
	m.srSamples++
	m.rtpTs = sr.RTPTime
	m.at = now
	m.packets = sr.PacketCount
	m.octets = sr.OctetCount
	m.received = true
}

// captureMs is the sender wall-clock time of an RTP timestamp, if known.
func (m *srMapping) captureMs(ts uint32, clockRate float64) (float64, bool) {
	if !m.received {
		return 0, false
	}
	return m.ntpMs + float64(int32(ts-m.rtpTs))/clockRate*1000, true
}

// delays turns d = arrival - capture into the one-way delay estimates
// described at the top of the file.
func (m *srMapping) delays(d float64, clk Clock) (owd, owdWall float64, haveWall bool) {
	half := 0.0
	if !math.IsNaN(clk.RTTMs) {
		half = clk.RTTMs / 2
	}
	return d - m.minSRMs + half, d - clk.OffsetMs, !math.IsNaN(clk.OffsetMs)
}

func (m *srMapping) snapshot(clk Clock) map[string]any {
	if !m.received {
		return nil
	}
	out := map[string]any{
		"senderNtpMs": m.ntpMs, "rtpTimestamp": m.rtpTs, "packetCount": m.packets, "octetCount": m.octets,
		"msSinceReceived":      float64(time.Since(m.at).Microseconds()) / 1000,
		"minArrivalMinusNtpMs": m.minSRMs, "reports": m.srSamples,
	}
	if !math.IsNaN(clk.RTTMs) && !math.IsNaN(clk.OffsetMs) {
		// How far the browser's RTCP NTP clock is from its Date.now().
		out["ntpMinusWallClockMs"] = clk.OffsetMs - (m.minSRMs - clk.RTTMs/2)
	}
	return out
}

func ntpToUnixMs(ntp uint64) float64 {
	const ntpEpochOffset = 2208988800
	secs := float64(ntp>>32) - ntpEpochOffset
	frac := float64(ntp&0xffffffff) / (1 << 32)
	return (secs + frac) * 1000
}

func msOf(t time.Time) float64 { return float64(t.UnixMicro()) / 1000 }

// seqTracker counts received, duplicate and lost packets from sequence numbers.
type seqTracker struct {
	started  bool
	base     uint64
	highest  uint64 // extended
	seen     map[uint64]struct{}
	unique   int64
	dups     int64
	reorders int64
}

func (t *seqTracker) observe(seq uint16) (ext uint64, dup bool) {
	if !t.started {
		t.started = true
		t.base = uint64(seq)
		t.highest = uint64(seq)
		t.seen = map[uint64]struct{}{}
		ext = uint64(seq)
	} else {
		// Pick the extension closest to the highest sequence number seen.
		ext = t.highest&^0xffff | uint64(seq)
		if ext+0x8000 < t.highest {
			ext += 0x10000
		} else if ext > t.highest+0x8000 && ext >= 0x10000 {
			ext -= 0x10000
		}
	}
	if _, ok := t.seen[ext]; ok {
		t.dups++
		return ext, true
	}
	t.seen[ext] = struct{}{}
	t.unique++
	if ext > t.highest {
		t.highest = ext
	} else if ext < t.highest {
		t.reorders++ // late arrival or NACK retransmission
	}
	if len(t.seen) > 8192 {
		for k := range t.seen {
			if k+4096 < t.highest {
				delete(t.seen, k)
			}
		}
	}
	return ext, false
}

func (t *seqTracker) expected() int64 {
	if !t.started {
		return 0
	}
	return int64(t.highest-t.base) + 1
}

func (t *seqTracker) lost() int64 {
	if l := t.expected() - t.unique; l > 0 {
		return l
	}
	return 0
}

type pendingFrame struct {
	ts                uint32
	firstAt, lastAt   time.Time
	minSeq, maxSeq    uint64
	packets           int
	bytes             int
	marker, startSeen bool
	key               bool
	width, height     int
}

// VideoReceiverStats reassembles video frames and accumulates receive-side QoE.
type VideoReceiverStats struct {
	mu        sync.Mutex
	codec     string // "h264", "vp8", "vp9"
	clockRate float64
	created   time.Time

	seq        seqTracker
	markerSeqs map[uint64]struct{}
	pending    map[uint32]*pendingFrame
	sr         srMapping

	// cumulative
	packets, bytes                int64
	frames, keyframes, lostFrames int64
	lastFrameTs                   uint32
	lastFrameAt                   time.Time
	haveLastFrame                 bool
	firstFrameAt, firstKeyframeAt time.Time
	gaps                          []float64 // recent inter-frame gaps (ms), for the freeze threshold
	freezes                       int64
	freezeMs, longestFreezeMs     float64
	receivingSince                time.Time
	minDelayMs                    float64 // min(completion - capture), for delay variation
	haveMinDelay                  bool
	width, height                 int
	needKeyframe                  bool
	lastPacketAt                  time.Time
	firstPacketAt                 time.Time

	// window
	winStart                                  time.Time
	winFrames, winKey, winLost, winPackets    int64
	winBytes                                  int64
	winMaxFrame                               int
	winAssembly, winGaps, winOWD, winDelayVar []float64
	winOWDWall                                []float64
	clock                                     Clock
	winLostPkts0, winDups0                    int64
	winFreezes                                int64
	winFreezeMs                               float64
}

// NewVideoReceiverStats creates an accumulator for one video stream.
func NewVideoReceiverStats(codec string, clockRate uint32) *VideoReceiverStats {
	now := time.Now()
	return &VideoReceiverStats{
		codec: codec, clockRate: float64(clockRate), created: now, winStart: now,
		markerSeqs: map[uint64]struct{}{}, pending: map[uint32]*pendingFrame{},
		needKeyframe: true,
	}
}

// FrameEvent describes something notable that Packet observed.
type FrameEvent struct {
	FirstFrame, FirstKeyframe bool
	FrameLost                 bool
	Freeze                    float64 // ms, 0 if none
}

// Packet records one received RTP packet.
func (s *VideoReceiverStats) Packet(p *rtp.Packet, size int, now time.Time, clk Clock) FrameEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock = clk
	var ev FrameEvent
	s.lastPacketAt = now
	if s.firstPacketAt.IsZero() {
		s.firstPacketAt = now
	}
	ext, dup := s.seq.observe(p.SequenceNumber)
	if dup {
		return ev
	}
	s.packets++
	s.bytes += int64(size)
	s.winPackets++
	s.winBytes += int64(size)
	if p.Marker {
		s.markerSeqs[ext] = struct{}{}
		if len(s.markerSeqs) > 1024 {
			for k := range s.markerSeqs {
				if k+2048 < s.seq.highest {
					delete(s.markerSeqs, k)
				}
			}
		}
	}

	// Ignore packets of frames we already completed or gave up on.
	if s.haveLastFrame && int32(p.Timestamp-s.lastFrameTs) <= 0 {
		return ev
	}
	f := s.pending[p.Timestamp]
	if f == nil {
		f = &pendingFrame{ts: p.Timestamp, firstAt: now, minSeq: ext, maxSeq: ext}
		s.pending[p.Timestamp] = f
	}
	f.lastAt = now
	f.packets++
	f.bytes += len(p.Payload)
	f.minSeq = min(f.minSeq, ext)
	f.maxSeq = max(f.maxSeq, ext)
	if p.Marker {
		f.marker = true
	}
	start, key, w, h := s.inspect(p.Payload)
	if start && ext == f.minSeq {
		f.startSeen = true
	}
	if key {
		f.key = true
	}
	if w > 0 {
		f.width, f.height = w, h
	}

	// Complete frames in RTP timestamp order. An incomplete frame blocks the
	// ones behind it until NACK has had time to repair it; after that it is
	// counted as lost.
	for {
		next := s.oldestPending()
		if next == nil {
			break
		}
		if s.complete(next) {
			s.finish(next, &ev)
			continue
		}
		if !s.newerCompleteSince(next, now) && now.Sub(next.firstAt) < staleFrameAfter {
			break
		}
		delete(s.pending, next.ts)
		s.lostFrames++
		s.winLost++
		s.needKeyframe = true
		s.lastFrameTs, s.haveLastFrame = next.ts, true
		ev.FrameLost = true
	}
	return ev
}

// newerCompleteSince reports whether a frame after f finished arriving more
// than lostFrameAfter ago.
func (s *VideoReceiverStats) newerCompleteSince(f *pendingFrame, now time.Time) bool {
	for _, pf := range s.pending {
		if int32(pf.ts-f.ts) > 0 && s.complete(pf) && now.Sub(pf.lastAt) > lostFrameAfter {
			return true
		}
	}
	return false
}

func (s *VideoReceiverStats) oldestPending() *pendingFrame {
	var o *pendingFrame
	for _, f := range s.pending {
		if o == nil || int32(f.ts-o.ts) < 0 {
			o = f
		}
	}
	return o
}

func (s *VideoReceiverStats) complete(f *pendingFrame) bool {
	if !f.marker || uint64(f.packets) != f.maxSeq-f.minSeq+1 {
		return false
	}
	if f.startSeen {
		return true
	}
	// The packet before the frame's first packet ended the previous frame.
	_, ok := s.markerSeqs[f.minSeq-1]
	return ok
}

func (s *VideoReceiverStats) finish(f *pendingFrame, ev *FrameEvent) {
	delete(s.pending, f.ts)
	// Waiting for a keyframe after loss: delta frames can't be decoded, count them as lost.
	if s.needKeyframe && !f.key && s.keyframes > 0 {
		s.lostFrames++
		s.winLost++
		s.lastFrameTs = f.ts
		s.haveLastFrame = true
		return
	}
	if f.key {
		s.needKeyframe = false
		s.keyframes++
		s.winKey++
		if s.firstKeyframeAt.IsZero() {
			s.firstKeyframeAt = f.lastAt
			ev.FirstKeyframe = true
		}
	} else if s.keyframes == 0 {
		// Nothing decodable yet.
		s.lastFrameTs = f.ts
		s.haveLastFrame = true
		return
	}
	if f.width > 0 {
		s.width, s.height = f.width, f.height
	}
	s.frames++
	s.winFrames++
	s.winMaxFrame = max(s.winMaxFrame, f.bytes)
	s.winAssembly = append(s.winAssembly, durMs(f.lastAt.Sub(f.firstAt)))
	if s.firstFrameAt.IsZero() {
		s.firstFrameAt = f.lastAt
		s.receivingSince = f.lastAt
		ev.FirstFrame = true
	}
	if s.haveLastFrame && !s.lastFrameAt.IsZero() {
		gap := durMs(f.lastAt.Sub(s.lastFrameAt))
		s.winGaps = append(s.winGaps, gap)
		if len(s.gaps) >= 5 {
			avg := mean(s.gaps)
			if gap > math.Max(3*avg, avg+150) {
				s.freezes++
				s.winFreezes++
				s.freezeMs += gap
				s.winFreezeMs += gap
				s.longestFreezeMs = math.Max(s.longestFreezeMs, gap)
				ev.Freeze = gap
			}
		}
		s.gaps = append(s.gaps, gap)
		if len(s.gaps) > gapHistory {
			s.gaps = s.gaps[1:]
		}
	}
	if capMs, ok := s.sr.captureMs(f.ts, s.clockRate); ok {
		d := msOf(f.lastAt) - capMs
		if !s.haveMinDelay || d < s.minDelayMs {
			s.minDelayMs, s.haveMinDelay = d, true
		}
		s.winDelayVar = append(s.winDelayVar, d-s.minDelayMs)
		owd, wall, ok := s.sr.delays(d, s.clock)
		s.winOWD = append(s.winOWD, owd)
		if ok {
			s.winOWDWall = append(s.winOWDWall, wall)
		}
	}
	s.lastFrameTs = f.ts
	s.lastFrameAt = f.lastAt
	s.haveLastFrame = true
}

// inspect reports whether the payload starts a frame and whether it carries a
// keyframe, plus the frame size when the payload header has it.
func (s *VideoReceiverStats) inspect(b []byte) (start, key bool, w, h int) {
	if len(b) == 0 {
		return
	}
	switch s.codec {
	case "h264":
		return h264FrameStart(b), h264Keyframe(b), 0, 0
	case "vp8":
		var p codecs.VP8Packet
		if _, err := p.Unmarshal(b); err != nil || len(p.Payload) == 0 {
			return
		}
		start = p.S == 1 && p.PID == 0
		if start && p.Payload[0]&0x01 == 0 {
			key = true
			if len(p.Payload) >= 10 {
				w = int(uint16(p.Payload[6])|uint16(p.Payload[7])<<8) & 0x3fff
				h = int(uint16(p.Payload[8])|uint16(p.Payload[9])<<8) & 0x3fff
			}
		}
		return
	case "vp9":
		var p codecs.VP9Packet
		if _, err := p.Unmarshal(b); err != nil {
			return
		}
		start = p.B
		key = p.B && !p.P
		if p.V && len(p.Width) > 0 {
			w, h = int(p.Width[len(p.Width)-1]), int(p.Height[len(p.Height)-1])
		}
		return
	}
	return
}

// h264FrameStart reports whether an RTP H264 payload begins an access unit:
// it starts with a parameter set, SEI or delimiter, or with the first slice of
// a picture (first_mb_in_slice == 0, i.e. the slice header's first bit is 1).
func h264FrameStart(b []byte) bool {
	first := func(t byte, next []byte) bool {
		switch t {
		case 6, 7, 8, 9:
			return true
		case 1, 5:
			return len(next) > 0 && next[0]&0x80 != 0
		}
		return false
	}
	switch t := b[0] & 0x1f; {
	case t >= 1 && t <= 23:
		return first(t, b[1:])
	case t == 24: // STAP-A: look at the first aggregated NAL
		return len(b) > 3 && first(b[3]&0x1f, b[4:])
	case t == 28: // FU-A start fragment
		return len(b) > 2 && b[1]&0x80 != 0 && first(b[1]&0x1f, b[2:])
	}
	return false
}

// h264Keyframe reports whether an RTP H264 payload contains an IDR slice or SPS.
func h264Keyframe(b []byte) bool {
	isKey := func(t byte) bool { return t == 5 || t == 7 }
	switch t := b[0] & 0x1f; {
	case t >= 1 && t <= 23:
		return isKey(t)
	case t == 24: // STAP-A
		for i := 1; i+2 < len(b); {
			n := int(b[i])<<8 | int(b[i+1])
			if i+2 < len(b) && isKey(b[i+2]&0x1f) {
				return true
			}
			i += 2 + n
		}
	case t == 28: // FU-A
		return len(b) > 1 && b[1]&0x80 != 0 && isKey(b[1]&0x1f)
	}
	return false
}

// SenderReport records an RTCP SR for this stream.
func (s *VideoReceiverStats) SenderReport(sr *rtcp.SenderReport) {
	s.mu.Lock()
	s.sr.observe(sr)
	s.mu.Unlock()
}

// NeedKeyframe reports whether a PLI should be sent: media is arriving but
// nothing decodable can be shown until a keyframe arrives.
func (s *VideoReceiverStats) NeedKeyframe() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.needKeyframe || s.firstPacketAt.IsZero() {
		return false
	}
	// At startup the browser sends a keyframe anyway; only ask if one is lost or late.
	return s.lostFrames > 0 || time.Since(s.firstPacketAt) > time.Second
}

// Window returns a snapshot and resets the windowed counters.
func (s *VideoReceiverStats) Window() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	secs := now.Sub(s.winStart).Seconds()
	if secs <= 0 {
		secs = 1e-9
	}
	lost, dups := s.seq.lost(), s.seq.dups
	winLostPkts := lost - s.winLostPkts0
	recvSec := 0.0
	if !s.receivingSince.IsZero() {
		recvSec = now.Sub(s.receivingSince).Seconds()
	}
	out := map[string]any{
		"codec":              s.codec,
		"packetsReceived":    s.packets,
		"bytesReceived":      s.bytes,
		"packetsLost":        lost,
		"packetsExpected":    s.seq.expected(),
		"duplicatePackets":   dups,
		"reorderedPackets":   s.seq.reorders,
		"framesReceived":     s.frames,
		"keyframes":          s.keyframes,
		"framesLost":         s.lostFrames,
		"freezeCount":        s.freezes,
		"totalFreezesMs":     s.freezeMs,
		"longestFreezeMs":    s.longestFreezeMs,
		"receivingSec":       recvSec,
		"waitingForKeyframe": s.needKeyframe,
		"pendingFrames":      len(s.pending),
		"msSinceLastPacket":  sinceMs(now, s.lastPacketAt),
		"msSinceLastFrame":   sinceMs(now, s.lastFrameAt),
		"firstFrameMs":       sinceStart(s.created, s.firstFrameAt),
		"firstKeyframeMs":    sinceStart(s.created, s.firstKeyframeAt),
		"senderReport":       s.sr.snapshot(s.clock),
		"windowSec":          secs,
		"bitrate":            float64(s.winBytes*8) / secs,
		"fps":                float64(s.winFrames) / secs,
		"windowFrames":       s.winFrames,
		"windowKeyframes":    s.winKey,
		"windowFramesLost":   s.winLost,
		"windowPackets":      s.winPackets,
		"windowPacketsLost":  winLostPkts,
		"windowDuplicates":   dups - s.winDups0,
		"windowFreezes":      s.winFreezes,
		"windowFreezeMs":     s.winFreezeMs,
		"maxFrameBytes":      s.winMaxFrame,
		"assemblyMsMean":     mean(s.winAssembly),
		"assemblyMsMax":      pct(s.winAssembly, 1),
		"interFrameMsMean":   mean(s.winGaps),
		"interFrameMsStd":    std(s.winGaps),
		"interFrameMsMax":    pct(s.winGaps, 1),
	}
	if recvSec > 0 {
		out["freezeRatio"] = s.freezeMs / 1000 / recvSec
	}
	if exp := winLostPkts + s.winPackets; exp > 0 && winLostPkts > 0 {
		out["windowLossPct"] = 100 * float64(winLostPkts) / float64(exp)
	} else {
		out["windowLossPct"] = 0.0
	}
	if s.width > 0 {
		out["width"], out["height"] = s.width, s.height
	}
	if len(s.winOWD) > 0 {
		out["owdMsMean"], out["owdMsP95"], out["owdMsMax"] = mean(s.winOWD), pct(s.winOWD, 0.95), pct(s.winOWD, 1)
	}
	if len(s.winOWDWall) > 0 {
		out["owdWallMsMean"] = mean(s.winOWDWall)
	}
	if len(s.winDelayVar) > 0 {
		out["delayVarMsMean"], out["delayVarMsMax"] = mean(s.winDelayVar), pct(s.winDelayVar, 1)
	}
	s.winStart = now
	s.winFrames, s.winKey, s.winLost, s.winPackets, s.winBytes, s.winMaxFrame = 0, 0, 0, 0, 0, 0
	s.winFreezes, s.winFreezeMs = 0, 0
	s.winAssembly, s.winGaps, s.winOWD, s.winDelayVar = s.winAssembly[:0], s.winGaps[:0], s.winOWD[:0], s.winDelayVar[:0]
	s.winOWDWall = s.winOWDWall[:0]
	s.winLostPkts0, s.winDups0 = lost, dups
	return out
}

// AudioReceiverStats counts packets, loss, jitter and one-way delay for audio.
type AudioReceiverStats struct {
	mu        sync.Mutex
	clockRate float64
	seq       seqTracker
	sr        srMapping

	packets, bytes int64
	jitter         float64 // RFC 3550 interarrival jitter, in RTP units
	lastTransit    float64
	haveTransit    bool
	minDelayMs     float64
	haveMinDelay   bool
	lastPacketAt   time.Time

	winStart             time.Time
	winPackets, winBytes int64
	winLostPkts0         int64
	winOWD, winDelayVar  []float64
	winOWDWall           []float64
	clock                Clock
}

// NewAudioReceiverStats creates an accumulator for one audio stream.
func NewAudioReceiverStats(clockRate uint32) *AudioReceiverStats {
	return &AudioReceiverStats{clockRate: float64(clockRate), winStart: time.Now()}
}

// Packet records one received audio RTP packet.
func (s *AudioReceiverStats) Packet(p *rtp.Packet, size int, now time.Time, clk Clock) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock = clk
	s.lastPacketAt = now
	if _, dup := s.seq.observe(p.SequenceNumber); dup {
		return
	}
	s.packets++
	s.bytes += int64(size)
	s.winPackets++
	s.winBytes += int64(size)
	arrival := msOf(now) / 1000 * s.clockRate
	transit := arrival - float64(p.Timestamp)
	if s.haveTransit {
		d := math.Abs(transit - s.lastTransit)
		if d < s.clockRate { // ignore RTP timestamp wraps and discontinuities
			s.jitter += (d - s.jitter) / 16
		}
	}
	s.lastTransit, s.haveTransit = transit, true
	if capMs, ok := s.sr.captureMs(p.Timestamp, s.clockRate); ok {
		d := msOf(now) - capMs
		if !s.haveMinDelay || d < s.minDelayMs {
			s.minDelayMs, s.haveMinDelay = d, true
		}
		s.winDelayVar = append(s.winDelayVar, d-s.minDelayMs)
		owd, wall, ok := s.sr.delays(d, clk)
		s.winOWD = append(s.winOWD, owd)
		if ok {
			s.winOWDWall = append(s.winOWDWall, wall)
		}
	}
}

// SenderReport records an RTCP SR for this stream.
func (s *AudioReceiverStats) SenderReport(sr *rtcp.SenderReport) {
	s.mu.Lock()
	s.sr.observe(sr)
	s.mu.Unlock()
}

// Window returns a snapshot and resets the windowed counters.
func (s *AudioReceiverStats) Window() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	secs := now.Sub(s.winStart).Seconds()
	if secs <= 0 {
		secs = 1e-9
	}
	lost := s.seq.lost()
	winLost := lost - s.winLostPkts0
	out := map[string]any{
		"packetsReceived":   s.packets,
		"bytesReceived":     s.bytes,
		"packetsLost":       lost,
		"duplicatePackets":  s.seq.dups,
		"jitterMs":          s.jitter / s.clockRate * 1000,
		"msSinceLastPacket": sinceMs(now, s.lastPacketAt),
		"senderReport":      s.sr.snapshot(s.clock),
		"windowSec":         secs,
		"bitrate":           float64(s.winBytes*8) / secs,
		"windowPackets":     s.winPackets,
		"windowPacketsLost": winLost,
		"windowLossPct":     0.0,
	}
	if winLost > 0 {
		out["windowLossPct"] = 100 * float64(winLost) / float64(winLost+s.winPackets)
	}
	if len(s.winOWD) > 0 {
		out["owdMsMean"], out["owdMsP95"] = mean(s.winOWD), pct(s.winOWD, 0.95)
	}
	if len(s.winOWDWall) > 0 {
		out["owdWallMsMean"] = mean(s.winOWDWall)
	}
	if len(s.winDelayVar) > 0 {
		out["delayVarMsMean"], out["delayVarMsMax"] = mean(s.winDelayVar), pct(s.winDelayVar, 1)
	}
	s.winStart = now
	s.winPackets, s.winBytes = 0, 0
	s.winLostPkts0 = lost
	s.winOWD, s.winDelayVar, s.winOWDWall = s.winOWD[:0], s.winDelayVar[:0], s.winOWDWall[:0]
	return out
}

func durMs(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func sinceStart(start, t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return durMs(t.Sub(start))
}

func std(v []float64) float64 {
	if len(v) < 2 {
		return 0
	}
	m := mean(v)
	s := 0.0
	for _, x := range v {
		s += (x - m) * (x - m)
	}
	return math.Sqrt(s / float64(len(v)))
}
