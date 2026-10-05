package session

import (
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/h264writer"
	"github.com/pion/webrtc/v4/pkg/media/ivfwriter"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"

	"puffer-webrtc/internal/telemetry"
)

// Uplink sessions: the browser sends a synthetic video (and tone), the server
// receives it, sends NACK/RR/TWCC/PLI feedback, and measures it without
// decoding (see telemetry.VideoReceiverStats).

const pliInterval = 500 * time.Millisecond

// uplinkState is the receive side of an uplink session. Fields set in OnTrack
// are atomic because the telemetry loop reads them concurrently.
type uplinkState struct {
	video      atomic.Pointer[telemetry.VideoReceiverStats]
	audio      atomic.Pointer[telemetry.AudioReceiverStats]
	videoSSRC  atomic.Uint32
	audioSSRC  atomic.Uint32
	videoCodec atomic.Value            // string
	audioCodec atomic.Value            // string
	rtcpVideo  *telemetry.RTCPCounters // RTCP received from the browser (SRs, SDES, ...)
	rtcpAudio  *telemetry.RTCPCounters
	plisSent   atomic.Int64
	lastPLI    atomic.Int64 // unix nanos

	recMu     sync.Mutex
	recorders []*recorder
}

func (u *uplinkState) closeRecorders() {
	u.recMu.Lock()
	defer u.recMu.Unlock()
	for _, r := range u.recorders {
		r.close()
	}
}

// configureUplink registers the codecs the browser may send and the
// receive-side TWCC feedback that drives the browser's congestion control.
func configureUplink(me *webrtc.MediaEngine, ir *interceptor.Registry) error {
	videoFB := []webrtc.RTCPFeedback{
		{Type: "goog-remb"}, {Type: "ccm", Parameter: "fir"}, {Type: "nack"},
		{Type: "nack", Parameter: "pli"}, {Type: webrtc.TypeRTCPFBTransportCC},
	}
	video := []webrtc.RTPCodecParameters{
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine: h264Fmtp, RTCPFeedback: videoFB}, PayloadType: 102},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f", RTCPFeedback: videoFB}, PayloadType: 127},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000,
			RTCPFeedback: videoFB}, PayloadType: 96},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP9, ClockRate: 90000,
			SDPFmtpLine: "profile-id=0", RTCPFeedback: videoFB}, PayloadType: 98},
	}
	for _, c := range video {
		if err := me.RegisterCodec(c, webrtc.RTPCodecTypeVideo); err != nil {
			return err
		}
	}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
			SDPFmtpLine: "minptime=10;useinbandfec=1", RTCPFeedback: []webrtc.RTCPFeedback{{Type: webrtc.TypeRTCPFBTransportCC}}},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return err
	}
	return webrtc.ConfigureTWCCSender(me, ir)
}

func (s *Session) addUplinkTransceivers() error {
	s.up.rtcpVideo = telemetry.NewRTCPCounters(0)
	s.up.rtcpAudio = telemetry.NewRTCPCounters(0)
	for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if _, err := s.pc.AddTransceiverFromKind(kind, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			return err
		}
	}
	s.pc.OnTrack(s.onUplinkTrack)
	return nil
}

func (s *Session) onUplinkTrack(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	c := track.Codec()
	codec := strings.ToLower(strings.TrimPrefix(c.MimeType, "video/"))
	codec = strings.TrimPrefix(codec, "audio/")
	ssrc := uint32(track.SSRC())
	s.event("track", fmt.Sprintf("%s %s ssrc=%d pt=%d %s", track.Kind(), c.MimeType, ssrc, c.PayloadType, c.SDPFmtpLine))
	rec := s.newRecorder(c)

	switch track.Kind() {
	case webrtc.RTPCodecTypeVideo:
		st := telemetry.NewVideoReceiverStats(codec, c.ClockRate)
		s.up.videoSSRC.Store(ssrc)
		s.up.videoCodec.Store(c.MimeType)
		s.up.video.Store(st)
		go s.readUplinkRTCP(receiver, s.up.rtcpVideo, st.SenderReport)
		go s.readUplinkVideo(track, st, rec)
	case webrtc.RTPCodecTypeAudio:
		st := telemetry.NewAudioReceiverStats(c.ClockRate)
		s.up.audioSSRC.Store(ssrc)
		s.up.audioCodec.Store(c.MimeType)
		s.up.audio.Store(st)
		go s.readUplinkRTCP(receiver, s.up.rtcpAudio, st.SenderReport)
		go s.readUplinkAudio(track, st, rec)
	}
}

func (s *Session) clockOffsetMs() float64 { return math.Float64frombits(s.clockOffset.Load()) }

func (s *Session) clock() telemetry.Clock {
	return telemetry.Clock{OffsetMs: s.clockOffsetMs(), RTTMs: math.Float64frombits(s.clockRTT.Load())}
}

func (s *Session) readUplinkVideo(track *webrtc.TrackRemote, st *telemetry.VideoReceiverStats, rec *recorder) {
	buf := make([]byte, 1500)
	for {
		n, _, err := track.Read(buf)
		if err != nil {
			return
		}
		now := time.Now()
		var p rtp.Packet
		if err := p.Unmarshal(buf[:n]); err != nil {
			continue
		}
		ev := st.Packet(&p, n, now, s.clock())
		rec.write(&p)
		if ev.FirstKeyframe {
			s.event("uplink", "first keyframe")
		}
		if ev.FirstFrame {
			s.event("uplink", "first video frame")
		}
		if ev.Freeze > 0 {
			s.event("freeze", fmt.Sprintf("%.0f ms", ev.Freeze))
		}
		if ev.FrameLost {
			s.maybePLI()
		}
	}
}

func (s *Session) readUplinkAudio(track *webrtc.TrackRemote, st *telemetry.AudioReceiverStats, rec *recorder) {
	buf := make([]byte, 1500)
	for {
		n, _, err := track.Read(buf)
		if err != nil {
			return
		}
		now := time.Now()
		var p rtp.Packet
		if err := p.Unmarshal(buf[:n]); err != nil {
			continue
		}
		st.Packet(&p, n, now, s.clock())
		rec.write(&p)
	}
}

func (s *Session) readUplinkRTCP(receiver *webrtc.RTPReceiver, c *telemetry.RTCPCounters, onSR func(*rtcp.SenderReport)) {
	for {
		pkts, _, err := receiver.ReadRTCP()
		if err != nil {
			return
		}
		c.Observe(pkts)
		for _, p := range pkts {
			if sr, ok := p.(*rtcp.SenderReport); ok {
				onSR(sr)
			}
		}
	}
}

// maybePLI asks the browser for a keyframe, at most once per pliInterval.
func (s *Session) maybePLI() {
	ssrc := s.up.videoSSRC.Load()
	if ssrc == 0 {
		return
	}
	now := time.Now().UnixNano()
	last := s.up.lastPLI.Load()
	if now-last < int64(pliInterval) || !s.up.lastPLI.CompareAndSwap(last, now) {
		return
	}
	if err := s.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}}); err != nil {
		return
	}
	s.up.plisSent.Add(1)
}

// runUplink starts once the PeerConnection is connected. Media arrives through
// OnTrack; this keeps requesting keyframes while the stream is undecodable and
// streams telemetry.
func (s *Session) runUplink() {
	s.event("media", "uplink started")
	go func() {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-t.C:
			}
			if v := s.up.video.Load(); v != nil && v.NeedKeyframe() {
				s.maybePLI()
			}
		}
	}()
	go s.telemetryLoop()
}

func (s *Session) uplinkSnapshot() map[string]any {
	out := map[string]any{
		"plisSent":  s.up.plisSent.Load(),
		"rtcpRecv":  map[string]any{"video": s.up.rtcpVideo.Snapshot(), "audio": s.up.rtcpAudio.Snapshot()},
		"videoSsrc": s.up.videoSSRC.Load(),
		"audioSsrc": s.up.audioSSRC.Load(),
	}
	if c, ok := s.up.videoCodec.Load().(string); ok {
		out["videoCodec"] = c
	}
	if c, ok := s.up.audioCodec.Load().(string); ok {
		out["audioCodec"] = c
	}
	if clk := s.clock(); !math.IsNaN(clk.OffsetMs) {
		out["clockOffsetMs"], out["clockRttMs"] = clk.OffsetMs, clk.RTTMs
	}
	if v := s.up.video.Load(); v != nil {
		out["video"] = v.Window()
		out["interceptorStats"] = map[string]any{"video": s.inboundStats(s.up.videoSSRC.Load())}
	}
	if a := s.up.audio.Load(); a != nil {
		out["audio"] = a.Window()
		is, _ := out["interceptorStats"].(map[string]any)
		if is == nil {
			is = map[string]any{}
			out["interceptorStats"] = is
		}
		is["audio"] = s.inboundStats(s.up.audioSSRC.Load())
	}
	return out
}

// inboundStats is the receive-side counterpart of interceptorStats.
func (s *Session) inboundStats(ssrc uint32) map[string]any {
	st := s.statsGetter.Get(ssrc)
	if st == nil {
		return nil
	}
	in := st.InboundRTPStreamStats
	return map[string]any{
		"inbound":        in,
		"remoteOutbound": st.RemoteOutboundRTPStreamStats,
		"jitterMs":       in.Jitter * 1000,
	}
}

// recorder saves a received stream when --uplink-record is set. A nil
// recorder ignores writes.
type recorder struct {
	mu sync.Mutex
	w  interface {
		WriteRTP(*rtp.Packet) error
		Close() error
	}
	path string
	errs int
}

func (s *Session) newRecorder(c webrtc.RTPCodecParameters) *recorder {
	if !s.cfg.UplinkRecord {
		return nil
	}
	stem := strings.TrimSuffix(s.log.Path, ".jsonl")
	var (
		w interface {
			WriteRTP(*rtp.Packet) error
			Close() error
		}
		path string
		err  error
	)
	switch strings.ToLower(c.MimeType) {
	case strings.ToLower(webrtc.MimeTypeH264):
		path = stem + ".h264"
		w, err = h264writer.New(path)
	case strings.ToLower(webrtc.MimeTypeVP8), strings.ToLower(webrtc.MimeTypeVP9):
		path = stem + ".ivf"
		w, err = ivfwriter.New(path, ivfwriter.WithCodec(c.MimeType))
	case strings.ToLower(webrtc.MimeTypeOpus):
		path = stem + ".ogg"
		w, err = oggwriter.New(path, c.ClockRate, c.Channels)
	default:
		return nil
	}
	if err != nil {
		log.Printf("[%s] record %s: %v", s.id, c.MimeType, err)
		return nil
	}
	r := &recorder{w: w, path: path}
	s.up.recMu.Lock()
	s.up.recorders = append(s.up.recorders, r)
	s.up.recMu.Unlock()
	s.event("recording", path)
	return r
}

func (r *recorder) write(p *rtp.Packet) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.w == nil {
		return
	}
	if err := r.w.WriteRTP(p); err != nil {
		r.errs++
	}
}

func (r *recorder) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.w != nil {
		_ = r.w.Close()
		r.w = nil
	}
}
