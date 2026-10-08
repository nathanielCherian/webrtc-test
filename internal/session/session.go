// Package session owns one viewer: its PeerConnection, the ffmpeg decode
// pipeline, the x264 encoder driven by GCC, and the telemetry stream.
package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"puffer-webrtc/internal/ratecontrol"
	"puffer-webrtc/internal/telemetry"
	"puffer-webrtc/internal/x264"
)

// ErrTooManySessions is returned when --max-sessions is reached.
var ErrTooManySessions = errors.New("too many active sessions")

// ErrBadOffer wraps problems with the offer request itself (reported as 400).
var ErrBadOffer = errors.New("bad offer")

// Config holds per-session tunables (set from flags).
type Config struct {
	StartBitrate, MinBitrate, MaxBitrate int // bps, GCC limits
	AudioBitrate                         int // bps, Opus
	Headroom                             float64
	KeyIntSec                            float64 // 0 = IDR only on PLI/FIR
	X264Preset                           string
	X264Threads                          int
	TelemetryInterval                    time.Duration
	FullStatsEvery                       int // send pc.GetStats() every N telemetry ticks
	LogDir                               string
	ConnectTimeout                       time.Duration
	MaxSessions                          int
	ICEServers                           []webrtc.ICEServer
	UplinkRecord                         bool // save received uplink streams next to the session log
}

// Manager creates and tracks sessions.
type Manager struct {
	cfg      Config
	info     *MediaInfo // default video
	lib      *Library
	settings webrtc.SettingEngine
	host     map[string]any

	mu       sync.Mutex
	sessions map[string]*Session
}

// NewManager creates a manager. settings is copied into every session's API.
// Downlink sessions pick their video from lib (lib.Default if none is given).
func NewManager(cfg Config, lib *Library, settings webrtc.SettingEngine, host map[string]any) *Manager {
	return &Manager{cfg: cfg, info: lib.Default, lib: lib, settings: settings, host: host, sessions: map[string]*Session{}}
}

// ActiveSessions returns the number of live sessions.
func (m *Manager) ActiveSessions() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// OfferRequest is the body of POST /offer.
type OfferRequest struct {
	SDP    string          `json:"sdp"`
	Type   string          `json:"type"`
	Client json.RawMessage `json:"client,omitempty"` // arbitrary client metadata, logged
	Mode   string          `json:"mode,omitempty"`   // "" or "downlink": server sends; "uplink": browser sends
	// Video is a file name from the media library; "" means the --video default.
	Video string `json:"video,omitempty"`
	// RateControl picks the downlink bandwidth estimator; nil means GCC.
	RateControl *RateControlRequest `json:"rateControl,omitempty"`
}

// RateControlRequest selects and parameterises the downlink rate control.
type RateControlRequest struct {
	Algorithm     string  `json:"algorithm"` // "gcc" (default) or "sine"
	CenterKbps    float64 `json:"centerKbps,omitempty"`
	AmplitudeKbps float64 `json:"amplitudeKbps,omitempty"`
	PeriodSec     float64 `json:"periodSec,omitempty"`
}

// Rate control algorithms.
const (
	RateControlGCC  = "gcc"
	RateControlSine = "sine"
)

// ModeUplink is OfferRequest.Mode for sessions where the browser sends media.
const ModeUplink = "uplink"

// OfferResponse is returned from POST /offer.
type OfferResponse struct {
	SDP       string `json:"sdp"`
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
}

// Session is one viewer (downlink) or one sender (uplink).
type Session struct {
	id      string
	mode    string
	rc      string                  // RateControlGCC or RateControlSine (downlink only)
	sine    *ratecontrol.SineParams // set when rc == RateControlSine
	mgr     *Manager
	cfg     Config
	info    *MediaInfo
	created time.Time
	ctx     context.Context
	cancel  context.CancelFunc

	pc          *webrtc.PeerConnection
	bwe         cc.BandwidthEstimator
	statsGetter stats.Getter
	videoTrack  *webrtc.TrackLocalStaticSample
	audioTrack  *webrtc.TrackLocalStaticSample
	videoSSRC   uint32
	audioSSRC   uint32
	rtcpVideo   *telemetry.RTCPCounters
	rtcpAudio   *telemetry.RTCPCounters

	enc      *x264.Encoder
	encStats *telemetry.EncoderStats
	pipe     atomic.Pointer[Pipeline]
	forceIDR atomic.Bool
	started  atomic.Bool
	dc       atomic.Pointer[webrtc.DataChannel]
	log      *telemetry.Logger

	up          uplinkState
	clockOffset atomic.Uint64 // float64 bits: server minus client wall clock (ms), NaN until the client reports one
	clockRTT    atomic.Uint64 // float64 bits: RTT of the client's best clock ping (ms)

	evMu   sync.Mutex
	events []map[string]any // connection state timeline, also sent to client

	closeOnce sync.Once
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// HandleOffer creates a session from a browser offer and returns the answer
// (with all ICE candidates included; no trickle).
func (m *Manager) HandleOffer(ctx context.Context, req OfferRequest, remoteAddr string) (*OfferResponse, error) {
	m.mu.Lock()
	if m.cfg.MaxSessions > 0 && len(m.sessions) >= m.cfg.MaxSessions {
		m.mu.Unlock()
		return nil, ErrTooManySessions
	}
	m.mu.Unlock()

	switch req.Mode {
	case "", "downlink":
		req.Mode = "downlink"
	case ModeUplink:
	default:
		return nil, fmt.Errorf("%w: unknown mode %q", ErrBadOffer, req.Mode)
	}
	rc, sine, err := m.parseRateControl(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadOffer, err)
	}
	info := m.info
	if req.Mode != ModeUplink {
		if info, err = m.lib.Get(ctx, req.Video); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrBadOffer, err)
		}
	}
	s := &Session{id: newID(), mode: req.Mode, rc: rc, sine: sine, mgr: m, cfg: m.cfg, info: info, created: time.Now(), encStats: telemetry.NewEncoderStats()}
	s.clockOffset.Store(math.Float64bits(math.NaN()))
	s.clockRTT.Store(math.Float64bits(math.NaN()))
	s.ctx, s.cancel = context.WithCancel(context.Background())
	logName := s.id
	if s.mode == ModeUplink {
		logName += "_uplink"
	}
	if s.log, err = telemetry.NewLogger(m.cfg.LogDir, logName); err != nil {
		return nil, fmt.Errorf("create log: %w", err)
	}
	s.log.Log("meta", "session", map[string]any{
		"sessionId": s.id, "mode": s.mode, "remoteAddr": remoteAddr, "client": req.Client,
		"media": s.info, "config": m.cfg, "host": m.host, "rateControl": s.rateControlInfo(),
	})

	if err := s.setup(ctx, req); err != nil {
		s.Close("setup failed: " + err.Error())
		return nil, err
	}
	m.mu.Lock()
	m.sessions[s.id] = s
	m.mu.Unlock()

	// Give up if ICE/DTLS never completes.
	go func() {
		select {
		case <-s.ctx.Done():
		case <-time.After(m.cfg.ConnectTimeout):
			if !s.started.Load() {
				s.Close("connect timeout")
			}
		}
	}()

	ld := s.pc.LocalDescription()
	s.log.Log("meta", "answer", map[string]any{"offer": req.SDP, "answer": ld.SDP})
	log.Printf("[%s] new %s session from %s (log: %s)", s.id, s.mode, remoteAddr, s.log.Path)
	return &OfferResponse{SDP: ld.SDP, Type: ld.Type.String(), SessionID: s.id}, nil
}

// parseRateControl validates the requested rate control. Uplink sessions
// ignore it.
func (m *Manager) parseRateControl(req OfferRequest) (string, *ratecontrol.SineParams, error) {
	if req.Mode == ModeUplink || req.RateControl == nil {
		return RateControlGCC, nil, nil
	}
	rc := req.RateControl
	switch rc.Algorithm {
	case "", RateControlGCC:
		return RateControlGCC, nil, nil
	case RateControlSine:
		p := ratecontrol.SineParams{
			CenterBps:    int(math.Round(rc.CenterKbps * 1000)),
			AmplitudeBps: int(math.Round(rc.AmplitudeKbps * 1000)),
			Period:       time.Duration(rc.PeriodSec * float64(time.Second)),
		}
		if err := p.Validate(m.cfg.MaxBitrate); err != nil {
			return "", nil, err
		}
		return RateControlSine, &p, nil
	default:
		return "", nil, fmt.Errorf("unknown rate control %q", rc.Algorithm)
	}
}

// rateControlInfo describes the configured rate control for logs and hello.
func (s *Session) rateControlInfo() map[string]any {
	if s.mode == ModeUplink {
		return nil
	}
	info := map[string]any{"algorithm": s.rc}
	if s.sine != nil {
		info["centerBps"] = s.sine.CenterBps
		info["amplitudeBps"] = s.sine.AmplitudeBps
		info["periodSec"] = s.sine.Period.Seconds()
	}
	return info
}

func (s *Session) setup(ctx context.Context, req OfferRequest) error {
	me := &webrtc.MediaEngine{}
	ir := &interceptor.Registry{}
	var bweCh chan cc.BandwidthEstimator
	var err error
	if s.mode == ModeUplink {
		err = configureUplink(me, ir)
	} else {
		bweCh, err = s.configureDownlink(me, ir)
	}
	if err != nil {
		return err
	}
	getterCh := make(chan stats.Getter, 1)
	statsFactory, err := stats.NewInterceptor()
	if err != nil {
		return err
	}
	statsFactory.OnNewPeerConnection(func(_ string, g stats.Getter) { getterCh <- g })
	// Before NACK/reports so it sees retransmissions and outgoing SRs (needed for RTT).
	ir.Add(statsFactory)
	if err := webrtc.ConfigureNack(me, ir); err != nil {
		return err
	}
	if err := webrtc.ConfigureRTCPReports(ir); err != nil {
		return err
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(ir),
		webrtc.WithSettingEngine(s.mgr.settings))
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: s.cfg.ICEServers})
	if err != nil {
		return err
	}
	s.pc = pc
	s.statsGetter = <-getterCh
	if s.mode == ModeUplink {
		err = s.addUplinkTransceivers()
	} else {
		s.bwe = <-bweCh
		err = s.addDownlinkTracks()
	}
	if err != nil {
		return err
	}

	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		s.event("pcState", st.String())
		switch st {
		case webrtc.PeerConnectionStateConnected:
			if s.started.CompareAndSwap(false, true) {
				if s.mode == ModeUplink {
					go s.runUplink()
				} else {
					go s.run()
				}
			}
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			s.Close("peer connection " + st.String())
		}
	})
	pc.OnICEConnectionStateChange(func(st webrtc.ICEConnectionState) { s.event("iceState", st.String()) })
	pc.OnICEGatheringStateChange(func(st webrtc.ICEGatheringState) { s.event("iceGatheringState", st.String()) })
	pc.OnSignalingStateChange(func(st webrtc.SignalingState) { s.event("signalingState", st.String()) })
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != "telemetry" {
			return
		}
		dc.OnOpen(func() {
			s.dc.Store(dc)
			s.event("dataChannel", "open")
			s.send(map[string]any{
				"type": "server-hello", "sessionId": s.id, "serverTimeMs": telemetry.NowMs(),
				"mode": s.mode, "media": s.info, "config": s.cfg, "host": s.mgr.host,
				"rateControl": s.rateControlInfo(), "videoSsrc": s.videoSSRC, "audioSsrc": s.audioSSRC, "logFile": s.log.Path,
			})
		})
		dc.OnClose(func() { s.event("dataChannel", "closed") })
		dc.OnMessage(func(msg webrtc.DataChannelMessage) { s.onClientMessage(msg.Data) })
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: req.SDP}); err != nil {
		return fmt.Errorf("set remote description: %w", err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("create answer: %w", err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("set local description: %w", err)
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Second):
		return errors.New("ICE gathering timed out")
	}
	return nil
}

// configureDownlink registers the send-side codecs and the bandwidth estimator
// (GCC, or the open-loop sine wave). It returns the channel that receives the
// session's bandwidth estimator.
func (s *Session) configureDownlink(me *webrtc.MediaEngine, ir *interceptor.Registry) (chan cc.BandwidthEstimator, error) {
	if err := registerCodecs(me); err != nil {
		return nil, err
	}
	// Order matters: later interceptors wrap earlier ones on the send path, so
	// packets flow NACK buffer -> stats -> TWCC seq numbering -> GCC pacer -> wire.
	bweCh := make(chan cc.BandwidthEstimator, 1)
	ccFactory, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		if s.sine != nil {
			return ratecontrol.NewSine(*s.sine), nil
		}
		return gcc.NewSendSideBWE(
			gcc.SendSideBWEInitialBitrate(s.cfg.StartBitrate),
			gcc.SendSideBWEMinBitrate(s.cfg.MinBitrate),
			gcc.SendSideBWEMaxBitrate(s.cfg.MaxBitrate),
		)
	})
	if err != nil {
		return nil, err
	}
	ccFactory.OnNewPeerConnection(func(_ string, e cc.BandwidthEstimator) { bweCh <- e })
	ir.Add(ccFactory)
	if err := webrtc.ConfigureTWCCHeaderExtensionSender(me, ir); err != nil {
		return nil, err
	}
	return bweCh, nil
}

// addDownlinkTracks adds the H264 and Opus tracks the server sends.
func (s *Session) addDownlinkTracks() error {
	var err error
	s.videoTrack, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: h264Fmtp,
	}, "video", "puffer")
	if err != nil {
		return err
	}
	s.audioTrack, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
	}, "audio", "puffer")
	if err != nil {
		return err
	}
	videoSender, err := s.pc.AddTrack(s.videoTrack)
	if err != nil {
		return err
	}
	audioSender, err := s.pc.AddTrack(s.audioTrack)
	if err != nil {
		return err
	}
	s.videoSSRC = uint32(videoSender.GetParameters().Encodings[0].SSRC)
	s.audioSSRC = uint32(audioSender.GetParameters().Encodings[0].SSRC)
	s.rtcpVideo = telemetry.NewRTCPCounters(s.videoSSRC)
	s.rtcpAudio = telemetry.NewRTCPCounters(s.audioSSRC)
	go s.readRTCP(videoSender, s.rtcpVideo, true)
	go s.readRTCP(audioSender, s.rtcpAudio, false)

	return nil
}

const h264Fmtp = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"

func registerCodecs(me *webrtc.MediaEngine) error {
	videoFB := []webrtc.RTCPFeedback{
		{Type: "goog-remb"}, {Type: "ccm", Parameter: "fir"}, {Type: "nack"},
		{Type: "nack", Parameter: "pli"}, {Type: webrtc.TypeRTCPFBTransportCC},
	}
	for _, c := range []webrtc.RTPCodecParameters{
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine: h264Fmtp, RTCPFeedback: videoFB}, PayloadType: 102},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f", RTCPFeedback: videoFB}, PayloadType: 127},
	} {
		if err := me.RegisterCodec(c, webrtc.RTPCodecTypeVideo); err != nil {
			return err
		}
	}
	return me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
			SDPFmtpLine: "minptime=10;useinbandfec=1", RTCPFeedback: []webrtc.RTCPFeedback{{Type: webrtc.TypeRTCPFBTransportCC}}},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio)
}

func (s *Session) readRTCP(sender *webrtc.RTPSender, c *telemetry.RTCPCounters, video bool) {
	for {
		pkts, _, err := sender.ReadRTCP()
		if err != nil {
			return
		}
		if c.Observe(pkts) && video {
			s.forceIDR.Store(true)
		}
	}
}

// run starts media once the PeerConnection is connected.
func (s *Session) run() {
	var err error
	keyint := 0
	if s.cfg.KeyIntSec > 0 {
		keyint = int(math.Round(s.cfg.KeyIntSec * s.info.FPS))
	}
	if sine, ok := s.bwe.(*ratecontrol.Sine); ok {
		sine.Start()
	}
	initial := s.videoBitrateFor(s.bwe.GetTargetBitrate())
	s.enc, err = x264.New(x264.Config{
		Width: s.info.Width, Height: s.info.Height, FPSNum: s.info.FPSNum, FPSDen: s.info.FPSDen,
		BitrateBps: initial, KeyInt: keyint, Threads: s.cfg.X264Threads, Preset: s.cfg.X264Preset,
	})
	if err != nil {
		s.Close("encoder: " + err.Error())
		return
	}
	go func() {
		<-s.ctx.Done()
		s.enc.Close()
	}()
	s.encStats.Bitrate(s.bwe.GetTargetBitrate(), initial)
	pipe, err := StartPipeline(s.ctx, s.info, s.cfg.AudioBitrate)
	if err != nil {
		s.Close("pipeline: " + err.Error())
		return
	}
	s.pipe.Store(pipe)
	s.event("media", "started")

	start := time.Now()
	go s.videoLoop(pipe, start)
	go s.audioLoop(pipe, start)
	go s.bitrateLoop()
	go s.telemetryLoop()
}

// videoBitrateFor converts a bandwidth estimator target (all streams, incl. overhead) into an
// encoder target.
func (s *Session) videoBitrateFor(target int) int {
	v := int(float64(target)*s.cfg.Headroom) - s.cfg.AudioBitrate
	if v < 30_000 {
		v = 30_000
	}
	return v
}

func (s *Session) bitrateLoop() {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		target := s.bwe.GetTargetBitrate()
		want := s.videoBitrateFor(target)
		cur := s.enc.Bitrate()
		if cur > 0 && math.Abs(float64(want-cur))/float64(cur) < 0.03 {
			continue
		}
		if err := s.enc.SetBitrate(want); err != nil {
			log.Printf("[%s] set bitrate: %v", s.id, err)
			continue
		}
		s.encStats.Bitrate(target, s.enc.Bitrate())
	}
}

func (s *Session) videoLoop(pipe *Pipeline, start time.Time) {
	frameDur := time.Duration(float64(time.Second) * float64(s.info.FPSDen) / float64(s.info.FPSNum))
	buf := make([]byte, s.enc.FrameSize())
	pending := time.Duration(0)
	for n := 0; ; n++ {
		if err := pipe.ReadVideoFrame(buf); err != nil {
			s.Close("video read: " + err.Error())
			return
		}
		if !sleepUntil(s.ctx, start.Add(time.Duration(n)*frameDur)) {
			return
		}
		pending += frameDur
		forced := s.forceIDR.Swap(false)
		t0 := time.Now()
		f, err := s.enc.Encode(buf, forced)
		encDur := time.Since(t0)
		if err != nil {
			s.Close("encode: " + err.Error())
			return
		}
		if f == nil {
			s.encStats.Skipped()
			continue
		}
		t1 := time.Now()
		if err := s.videoTrack.WriteSample(media.Sample{Data: f.Data, Duration: pending}); err != nil {
			s.Close("write video: " + err.Error())
			return
		}
		s.encStats.Frame(len(f.Data), f.Keyframe, forced && f.Keyframe, encDur, time.Since(t1))
		pending = 0
	}
}

func (s *Session) audioLoop(pipe *Pipeline, start time.Time) {
	r, err := pipe.OggReader()
	if err != nil {
		s.Close("audio open: " + err.Error())
		return
	}
	var lastGranule uint64
	var mediaT time.Duration
	for {
		page, hdr, err := r.ParseNextPage()
		if err != nil {
			s.Close("audio read: " + err.Error())
			return
		}
		pipe.AudioPages.Add(1)
		if bytes.HasPrefix(page, []byte("OpusTags")) {
			lastGranule = hdr.GranulePosition
			continue
		}
		dur := 20 * time.Millisecond
		if hdr.GranulePosition > lastGranule {
			if d := time.Duration(hdr.GranulePosition-lastGranule) * time.Second / 48000; d <= time.Second {
				dur = d
			}
		}
		lastGranule = hdr.GranulePosition
		if !sleepUntil(s.ctx, start.Add(mediaT)) {
			return
		}
		mediaT += dur
		if err := s.audioTrack.WriteSample(media.Sample{Data: page, Duration: dur}); err != nil {
			s.Close("write audio: " + err.Error())
			return
		}
	}
}

func sleepUntil(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *Session) telemetryLoop() {
	t := time.NewTicker(s.cfg.TelemetryInterval)
	defer t.Stop()
	for tick := 0; ; tick++ {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		full := s.cfg.FullStatsEvery > 0 && tick%s.cfg.FullStatsEvery == 0
		snap := s.snapshot(full)
		s.log.Log("server", "stats", snap)
		s.send(snap)
	}
}

// snapshot gathers everything the server knows about this session.
func (s *Session) snapshot(full bool) map[string]any {
	snap := map[string]any{
		"type":         "server-stats",
		"sessionId":    s.id,
		"mode":         s.mode,
		"serverTimeMs": telemetry.NowMs(),
		"uptimeSec":    time.Since(s.created).Seconds(),
		"state": map[string]any{
			"pc":        s.pc.ConnectionState().String(),
			"ice":       s.pc.ICEConnectionState().String(),
			"signaling": s.pc.SignalingState().String(),
		},
	}
	if s.mode == ModeUplink {
		snap["uplink"] = s.uplinkSnapshot()
	} else {
		s.downlinkSnapshot(snap)
	}
	if sctp := s.pc.SCTP(); sctp != nil && sctp.Transport() != nil && sctp.Transport().ICETransport() != nil {
		if pair, err := sctp.Transport().ICETransport().GetSelectedCandidatePair(); err == nil && pair != nil {
			snap["selectedCandidatePair"] = map[string]any{"local": pair.Local.String(), "remote": pair.Remote.String()}
		}
		snap["state"].(map[string]any)["dtls"] = sctp.Transport().State().String()
	}
	if dc := s.dc.Load(); dc != nil {
		snap["dataChannelBufferedBytes"] = dc.BufferedAmount()
	}
	if full {
		snap["pcStats"] = s.pc.GetStats()
		s.evMu.Lock()
		snap["events"] = append([]map[string]any(nil), s.events...)
		s.evMu.Unlock()
	}
	return snap
}

// downlinkSnapshot adds the rate control (and GCC), encoder and sender-side
// RTCP sections.
func (s *Session) downlinkSnapshot(snap map[string]any) {
	target := s.bwe.GetTargetBitrate()
	if s.rc == RateControlGCC {
		gccStats := map[string]any{"targetBitrate": target}
		for k, v := range s.bwe.GetStats() {
			gccStats[k] = v
		}
		snap["gcc"] = gccStats
		snap["rateControl"] = map[string]any{"algorithm": RateControlGCC, "targetBitrate": target}
	} else {
		rc := s.bwe.GetStats()
		rc["targetBitrate"] = target
		snap["rateControl"] = rc
	}
	enc := s.encStats.Window()
	enc["width"], enc["height"], enc["fps"] = s.info.Width, s.info.Height, s.info.FPS
	mediaInfo := map[string]any{}
	if p := s.pipe.Load(); p != nil {
		pos, loops := p.MediaPosition()
		mediaInfo = map[string]any{"positionSec": pos, "loops": loops,
			"framesRead": p.FramesRead.Load(), "audioPages": p.AudioPages.Load()}
	}
	snap["encoder"] = enc
	snap["media"] = mediaInfo
	snap["rtcp"] = map[string]any{"video": s.rtcpVideo.Snapshot(), "audio": s.rtcpAudio.Snapshot()}
	snap["interceptorStats"] = map[string]any{
		"video": s.interceptorStats(s.videoSSRC),
		"audio": s.interceptorStats(s.audioSSRC),
	}
}

// interceptorStats splits pion's stats.Stats into its parts: the embedded
// structs have overlapping field names that encoding/json would silently drop.
func (s *Session) interceptorStats(ssrc uint32) map[string]any {
	st := s.statsGetter.Get(ssrc)
	if st == nil {
		return nil
	}
	ri := st.RemoteInboundRTPStreamStats
	return map[string]any{
		"outbound":      st.OutboundRTPStreamStats,
		"remoteInbound": ri,
		// Convenience copies in milliseconds.
		"rttMs":        float64(ri.RoundTripTime.Microseconds()) / 1000,
		"jitterMs":     ri.Jitter * 1000,
		"fractionLost": ri.FractionLost,
	}
}

func (s *Session) onClientMessage(data []byte) {
	var head struct {
		Type     string   `json:"type"`
		ID       int64    `json:"id"`
		T1       float64  `json:"t1"`
		OffsetMs *float64 `json:"offsetMs"`
		RTTMs    *float64 `json:"rttMs"`
	}
	_ = json.Unmarshal(data, &head)
	switch head.Type {
	case "":
		head.Type = "unknown"
	case "clock-ping":
		// Answered immediately and not logged; the client reports its
		// resulting estimate with "clock-sync".
		s.send(map[string]any{"type": "clock-pong", "id": head.ID, "t1": head.T1, "t2": telemetry.NowMs()})
		return
	case "clock-sync":
		if head.OffsetMs != nil && head.RTTMs != nil {
			s.clockRTT.Store(math.Float64bits(*head.RTTMs))
			s.clockOffset.Store(math.Float64bits(*head.OffsetMs))
		}
	}
	s.log.Log("client", head.Type, json.RawMessage(data))
}

// send marshals v and sends it on the telemetry DataChannel, dropping the
// message if the channel is backed up (telemetry must not starve media).
func (s *Session) send(v any) {
	dc := s.dc.Load()
	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return
	}
	if dc.BufferedAmount() > 1<<20 {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("[%s] marshal telemetry: %v", s.id, err)
		return
	}
	_ = dc.SendText(string(b))
}

func (s *Session) event(kind, value string) {
	ev := map[string]any{"t": telemetry.NowMs(), "kind": kind, "value": value}
	s.evMu.Lock()
	s.events = append(s.events, ev)
	s.evMu.Unlock()
	s.log.Log("event", kind, ev)
	s.send(map[string]any{"type": "server-event", "event": ev})
}

// Close tears the session down; safe to call multiple times.
func (s *Session) Close(reason string) {
	s.closeOnce.Do(func() {
		log.Printf("[%s] closing: %s", s.id, reason)
		s.event("closed", reason)
		s.cancel()
		if p := s.pipe.Load(); p != nil {
			p.Close()
		}
		if s.pc != nil {
			_ = s.pc.Close()
		}
		s.up.closeRecorders()
		_ = s.log.Close()
		s.mgr.mu.Lock()
		delete(s.mgr.sessions, s.id)
		s.mgr.mu.Unlock()
	})
}

// CloseAll shuts down every session (server shutdown).
func (m *Manager) CloseAll() {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	for _, s := range all {
		s.Close("server shutdown")
	}
}
