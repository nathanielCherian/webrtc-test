package session

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"puffer-webrtc/internal/ratecontrol"
	"puffer-webrtc/internal/simulcast"
)

// Simulcast sessions forward the shared, pre-encoded renditions from a
// simulcast.Hub instead of running their own decoder and encoder. Each session
// still has its own PeerConnection and rate control (GCC or a wave); the rate
// control picks which rendition to forward, switching on the renditions'
// time-aligned IDRs, and pads with probe packets so GCC can discover that a
// higher rendition fits (or, with OfferRequest.Padding "fill", so the send rate
// always matches the GCC target).

// SourceSimulcast is OfferRequest.Source for the shared simulcast renditions.
const SourceSimulcast = "simulcast"

const (
	rtpPayloadMTU   = 1188 // same as Pion's TrackLocalStaticSample (1200 - RTP header)
	padTickInterval = 10 * time.Millisecond
	probeBackoff    = 3 * time.Second // no padding after GCC lowers its target
)

var errNoSimulcast = errors.New("simulcast source is not enabled on this server (--simulcast)")

// SetSimulcast enables simulcast sessions backed by hub.
func (m *Manager) SetSimulcast(hub *simulcast.Hub) { m.hub = hub }

// simulcastInfo describes the simulcast source in the MediaInfo shape the page
// and logs already understand. Width/height/fps are the top layer's.
func (m *Manager) simulcastInfo() (*MediaInfo, error) {
	if m.hub == nil {
		return nil, errNoSimulcast
	}
	st := m.hub.Status()
	live := false
	for _, l := range st {
		live = live || l.Live
	}
	if !live {
		return nil, errors.New("simulcast source has no live layers (is the transcoder sending to the --simulcast addresses?)")
	}
	top := st[0]
	fps := top.FPS
	if fps <= 0 {
		fps = 30
	}
	info := &MediaInfo{Path: SourceSimulcast, Width: top.Width, Height: top.Height,
		FPSNum: int(math.Round(fps * 1000)), FPSDen: 1000, FPS: fps, HasAudio: m.hub.HasAudio(), VideoCodec: "h264"}
	if info.HasAudio {
		info.AudioCodec = "opus"
	}
	return info, nil
}

// simState is the per-session simulcast forwarding state.
type simState struct {
	sub    *simulcast.Subscriber
	layers []simulcast.LayerConfig
	sel    *simulcast.Selector
	fill   bool // pad up to the whole GCC target instead of only probing

	mu          sync.Mutex // guards everything below (read by snapshots)
	cur         int        // layer being forwarded, -1 before the first keyframe
	pending     int        // layer to switch to at its next keyframe, -1 = none
	needKey     bool       // dropped frames (PLI, overflow): skip until the next keyframe
	budget      int
	target      int
	switches    int64
	keyWaits    int64 // PLI/FIR or overflow that made us wait for a keyframe
	waitFrames  int64 // frames of the current layer skipped while waiting
	padBps      int   // probe mode: rate to pad at; fill mode: measured padding rate
	fillBps     int   // fill mode: total send rate that media plus padding should reach
	padSent     rateMeter
	padBytes    int64
	padPackets  int64
	probeHold   time.Time
	sentBytes   rateMeter // video payload sent, for the padding computation
	startedAt   time.Time
	firstFrame  time.Time
	layerFrames []int64
}

func newSimState(hub *simulcast.Hub) *simState {
	layers := hub.Layers()
	rates := make([]int, len(layers))
	for i, l := range layers {
		rates[i] = l.BitrateBps
	}
	return &simState{layers: layers, sel: simulcast.NewSelector(rates), cur: -1, pending: -1,
		layerFrames: make([]int64, len(layers))}
}

func (st *simState) name(i int) string {
	if i < 0 || i >= len(st.layers) {
		return "none"
	}
	return st.layers[i].Name
}

func (st *simState) paddingMode() string {
	if st.fill {
		return PaddingFill
	}
	return PaddingProbe
}

// simulcastLayers lists the renditions for hello/meta (nil for other sessions).
func (s *Session) simulcastLayers() []simulcast.LayerConfig {
	if s.sim == nil {
		return nil
	}
	return s.sim.layers
}

// wrapEstimator adds padding-probe support to the session's estimator.
func (s *Session) wrapEstimator(e cc.BandwidthEstimator) cc.BandwidthEstimator {
	if s.sim == nil {
		return e
	}
	return ratecontrol.WithPaddingProbes(e)
}

// waveEstimator returns the session's wave estimator, if it uses one.
func (s *Session) waveEstimator() (*ratecontrol.Wave, bool) {
	e := s.bwe
	if p, ok := e.(*ratecontrol.PaddingProbes); ok {
		e = p.Unwrap()
	}
	w, ok := e.(*ratecontrol.Wave)
	return w, ok
}

// addSimulcastVideoTrack adds an RTP-level video track: the session packetizes
// itself so it controls timestamps across layer switches and can add probes.
func (s *Session) addSimulcastVideoTrack() (*webrtc.RTPSender, error) {
	var err error
	s.videoRTP, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: h264Fmtp,
	}, "video", "puffer")
	if err != nil {
		return nil, err
	}
	return s.pc.AddTrack(s.videoRTP)
}

// runSimulcast starts forwarding once the PeerConnection is connected.
func (s *Session) runSimulcast() {
	st := s.sim
	if w, ok := s.waveEstimator(); ok {
		w.Start()
	}
	st.sub = s.mgr.hub.Subscribe()
	st.startedAt = time.Now()
	target := s.bwe.GetTargetBitrate()
	st.mu.Lock()
	st.target, st.budget = target, s.videoBitrateFor(target)
	st.pending = st.sel.Decide(time.Now(), st.budget, -1)
	st.mu.Unlock()
	s.encStats.Bitrate(target, st.layers[st.pending].BitrateBps)
	s.event("media", fmt.Sprintf("started: simulcast, waiting for a %s keyframe", st.name(st.pending)))

	go s.simAudioLoop()
	go s.telemetryLoop()
	s.simVideoLoop()
}

func (s *Session) simAudioLoop() {
	sub := s.sim.sub
	for {
		select {
		case <-s.ctx.Done():
			return
		case a := <-sub.Audio:
			if err := s.audioTrack.WriteSample(media.Sample{Data: a.Data, Duration: a.Duration}); err != nil {
				s.Close("write audio: " + err.Error())
				return
			}
		}
	}
}

// simVideoLoop forwards frames, re-evaluates the layer every 100 ms and sends
// probes every 10 ms. All writes to the video track happen on this goroutine.
func (s *Session) simVideoLoop() {
	st := s.sim
	fw := &forwarder{s: s, seq: rtp.NewRandomSequencer(), pay: &codecs.H264Payloader{}, lastDelta: 3000}
	var b [4]byte
	_, _ = rand.Read(b[:])
	fw.ts = binary.BigEndian.Uint32(b[:])

	decide := time.NewTicker(100 * time.Millisecond)
	defer decide.Stop()
	pad := time.NewTicker(padTickInterval)
	defer pad.Stop()
	lastPad := time.Now()
	for {
		select {
		case <-s.ctx.Done():
			return
		case f := <-st.sub.Video:
			if err := fw.frame(f); err != nil {
				s.Close("write video: " + err.Error())
				return
			}
		case now := <-decide.C:
			s.simDecide(now)
		case now := <-pad.C:
			if err := fw.padding(now.Sub(lastPad)); err != nil {
				s.Close("write probe: " + err.Error())
				return
			}
			lastPad = now
		}
	}
}

// simDecide re-runs layer selection and the padding rate.
func (s *Session) simDecide(now time.Time) {
	st := s.sim
	target := s.bwe.GetTargetBitrate()
	budget := s.videoBitrateFor(target)

	st.mu.Lock()
	if st.target > 0 && float64(target) < 0.97*float64(st.target) {
		st.probeHold = now.Add(probeBackoff) // GCC backed off: stop probing for a while
	}
	st.target, st.budget = target, budget
	desired := st.sel.Decide(now, budget, st.cur)
	var ev string
	switch {
	case st.cur < 0:
		st.pending = desired
	case desired != st.cur && desired != st.pending:
		ev = fmt.Sprintf("%s -> %s at next keyframe (budget %d kbps)", st.name(st.cur), st.name(desired), budget/1000)
		st.pending = desired
	case desired == st.cur && st.pending >= 0:
		ev = fmt.Sprintf("cancel switch to %s (budget %d kbps)", st.name(st.pending), budget/1000)
		st.pending = -1
	}

	// Only GCC pads: a wave's target is open-loop.
	st.padBps, st.fillBps = 0, 0
	switch {
	case s.rc != RateControlGCC:
	case st.fill:
		// Fill mode: forwarder.padding sends whatever the media leaves of the target.
		st.fillBps = target
		st.padBps = int(st.padSent.rate(now))
	case st.cur > 0 && (st.pending < 0 || st.pending < st.cur) && now.After(st.probeHold):
		// Pad up to (just under) the estimate while a higher layer exists, so GCC can
		// grow beyond 1.5x what we send.
		next := float64(st.layers[st.cur-1].BitrateBps) * st.sel.UpMargin
		need := (next + float64(s.cfg.AudioBitrate)) / s.cfg.Headroom * 1.05
		goal := math.Min(0.95*float64(target), need)
		media := st.sentBytes.rate(now) + float64(s.cfg.AudioBitrate)
		st.padBps = int(math.Max(0, goal-media))
	}
	st.mu.Unlock()
	if ev != "" {
		s.event("layerPending", ev)
	}
}

// forwarder packetizes the frames of whichever layer is current onto one RTP
// stream with continuous sequence numbers and timestamps.
type forwarder struct {
	s         *Session
	seq       rtp.Sequencer
	pay       *codecs.H264Payloader
	ts        uint32 // RTP timestamp of the last frame sent
	lastPTS   int64
	lastDelta int64
	started   bool
	padCredit float64
	fillDebt  float64 // fill mode: video bytes sent that padding has not yet made room for
}

func (fw *forwarder) frame(f *simulcast.Frame) error {
	s, st := fw.s, fw.s.sim
	var ev string
	st.mu.Lock()
	if st.sub.Overflow.Swap(false) && st.cur >= 0 {
		st.needKey = true
		st.keyWaits++
		ev = "subscriber queue overflowed; waiting for a keyframe"
	}
	if s.forceIDR.Swap(false) && st.cur >= 0 && !st.needKey {
		// The encoder is shared, so a PLI/FIR can't force an IDR; skip to the next periodic one.
		st.needKey = true
		st.keyWaits++
	}
	switch {
	case st.cur < 0:
		if f.Layer != st.pending || !f.Key {
			st.mu.Unlock()
			return nil
		}
		st.cur, st.pending, st.firstFrame = st.pending, -1, time.Now()
		ev = fmt.Sprintf("forwarding %s (first keyframe after %.0f ms)", st.name(st.cur), float64(time.Since(st.startedAt).Microseconds())/1000)
	case st.pending >= 0 && f.Layer == st.pending && f.Key:
		ev = fmt.Sprintf("%s -> %s", st.name(st.cur), st.name(st.pending))
		st.cur, st.pending, st.needKey = st.pending, -1, false
		st.switches++
	}
	if f.Layer != st.cur {
		st.mu.Unlock()
		if ev != "" {
			s.event("layer", ev)
		}
		return nil
	}
	if st.needKey && !f.Key {
		st.waitFrames++
		st.mu.Unlock()
		return nil
	}
	st.needKey = false
	st.layerFrames[st.cur]++
	st.sentBytes.add(time.Now(), f.Bytes)
	layerBps, target := st.layers[st.cur].BitrateBps, st.target
	st.mu.Unlock()
	if ev != "" {
		s.event("layer", ev)
		s.encStats.Bitrate(target, layerBps)
	}

	// All layers share one 90 kHz clock, so PTS deltas carry straight over a switch.
	if fw.started {
		d := f.PTS - fw.lastPTS
		if d <= 0 || d > 10*90000 { // source restarted or jumped
			d = fw.lastDelta
		} else if d <= 9000 {
			fw.lastDelta = d
		}
		fw.ts += uint32(d)
	}
	fw.started, fw.lastPTS = true, f.PTS

	t0 := time.Now()
	payloads := fw.pay.Payload(rtpPayloadMTU, f.Data)
	for i, p := range payloads {
		pkt := &rtp.Packet{Header: rtp.Header{Version: 2, Marker: i == len(payloads)-1,
			SequenceNumber: fw.seq.NextSequenceNumber(), Timestamp: fw.ts}, Payload: p}
		if err := s.videoRTP.WriteRTP(pkt); err != nil {
			return err
		}
	}
	s.encStats.Frame(f.Bytes, f.Key, false, 0, time.Since(t0))
	fw.fillDebt += float64(f.Bytes)
	return nil
}

// padding sends probe packets: at the rate simDecide chose, or in fill mode
// whatever the target allowed since the last tick and the media did not use.
func (fw *forwarder) padding(dt time.Duration) error {
	st := fw.s.sim
	st.mu.Lock()
	rate, fill := st.padBps, st.fillBps
	st.mu.Unlock()
	switch {
	case !fw.started || (fill <= 0 && rate <= 0):
		fw.padCredit, fw.fillDebt = 0, 0
		return nil
	case fill > 0:
		// Token bucket: each tick earns the target's bytes less audio, and video
		// frames are paid for first. A large frame leaves a debt (at most 1 s of
		// the target) that pauses padding until the average is back on target.
		earned := float64(fill-fw.s.cfg.AudioBitrate) * math.Min(dt.Seconds(), 0.1) / 8
		fw.fillDebt = math.Min(fw.fillDebt-earned, float64(fill)/8)
		if fw.fillDebt < 0 {
			fw.padCredit -= fw.fillDebt
			fw.fillDebt = 0
		}
	default:
		fw.padCredit = math.Min(fw.padCredit+float64(rate)*dt.Seconds()/8, 8*ratecontrol.MaxProbeBytes)
	}
	var n, bytes int64
	for fw.padCredit >= ratecontrol.MaxProbeBytes {
		pkt := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: fw.seq.NextSequenceNumber(), Timestamp: fw.ts},
			Payload: ratecontrol.ProbePayload(ratecontrol.MaxProbeBytes)}
		if err := fw.s.videoRTP.WriteRTP(pkt); err != nil {
			return err
		}
		fw.padCredit -= ratecontrol.MaxProbeBytes
		n++
		bytes += ratecontrol.MaxProbeBytes
	}
	st.mu.Lock()
	st.padPackets += n
	st.padBytes += bytes
	st.padSent.add(time.Now(), int(bytes))
	st.mu.Unlock()
	return nil
}

// simSnapshot is the "simulcast" section of server telemetry.
func (s *Session) simSnapshot() map[string]any {
	st := s.sim
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	sentVideo := st.sentBytes.rate(now)
	out := map[string]any{
		"layer": st.name(st.cur), "pending": nil, "budgetBps": st.budget, "targetBps": st.target,
		"switches": st.switches, "keyframeWaits": st.keyWaits, "framesSkippedWaitingForKey": st.waitFrames,
		"paddingMode": st.paddingMode(), "paddingBps": st.padBps, "paddingBytes": st.padBytes, "paddingPackets": st.padPackets,
		"probeHoldMs":  math.Max(0, float64(time.Until(st.probeHold).Milliseconds())),
		"sentVideoBps": sentVideo,
		// Everything sent: video, audio (nominal) and the padding actually written.
		"sentTotalBps": sentVideo + float64(s.cfg.AudioBitrate) + st.padSent.rate(now),
	}
	if st.pending >= 0 {
		out["pending"] = st.name(st.pending)
	}
	if st.sub != nil {
		out["subscriberDrops"] = st.sub.Dropped.Load()
	}
	frames := map[string]int64{}
	for i, l := range st.layers {
		frames[l.Name] = st.layerFrames[i]
	}
	out["framesByLayer"] = frames
	if !st.firstFrame.IsZero() {
		out["startupMs"] = float64(st.firstFrame.Sub(st.startedAt).Microseconds()) / 1000
	}
	return out
}

// rateMeter measures a byte rate over the last second in 100 ms buckets.
type rateMeter struct {
	buckets [10]int
	stamps  [10]int64
}

func (w *rateMeter) add(now time.Time, n int) {
	slot := now.UnixMilli() / 100
	i := slot % 10
	if w.stamps[i] != slot {
		w.stamps[i], w.buckets[i] = slot, 0
	}
	w.buckets[i] += n
}

// rate returns bits per second over the last full second.
func (w *rateMeter) rate(now time.Time) float64 {
	slot := now.UnixMilli() / 100
	total := 0
	for i := range w.buckets {
		if age := slot - w.stamps[i]; age >= 1 && age <= 10 {
			total += w.buckets[i]
		}
	}
	return float64(total * 8)
}
