// Package ratecontrol holds open-loop bandwidth estimators that replace GCC
// for experiments. They plug into Pion's cc interceptor like GCC does, so the
// session's encoder and pacer follow them instead.
package ratecontrol

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/rtcp"
)

// SineParams describes rate(t) = Center + Amplitude*sin(2*pi*t/Period).
type SineParams struct {
	CenterBps    int           `json:"centerBps"`
	AmplitudeBps int           `json:"amplitudeBps"`
	Period       time.Duration `json:"period"`
}

// Validate checks that the rate stays positive and below maxBps.
func (p SineParams) Validate(maxBps int) error {
	switch {
	case p.CenterBps <= 0:
		return errors.New("sine center must be > 0")
	case p.AmplitudeBps < 0 || p.AmplitudeBps >= p.CenterBps:
		return errors.New("sine amplitude must be >= 0 and < center")
	case p.Period < time.Second:
		return errors.New("sine period must be >= 1 s")
	case maxBps > 0 && p.CenterBps+p.AmplitudeBps > maxBps:
		return fmt.Errorf("sine peak %d bps exceeds --max-bitrate %d", p.CenterBps+p.AmplitudeBps, maxBps)
	}
	return nil
}

// RateAt returns the rate elapsed into the wave.
func (p SineParams) RateAt(elapsed time.Duration) int {
	phase := 2 * math.Pi * elapsed.Seconds() / p.Period.Seconds()
	return p.CenterBps + int(math.Round(float64(p.AmplitudeBps)*math.Sin(phase)))
}

// pacerUpdateInterval is how often the pacer rate follows the wave.
const pacerUpdateInterval = 20 * time.Millisecond

// Sine is a cc.BandwidthEstimator whose target follows a sine wave and
// ignores feedback. Packets go through the same leaky bucket pacer GCC uses.
type Sine struct {
	p     SineParams
	now   func() time.Time
	pacer gcc.Pacer

	mu       sync.Mutex
	t0       time.Time // zero until Start
	onChange func(int)

	twccFeedback atomic.Int64
	rtcpPackets  atomic.Int64
	done         chan struct{}
	closeOnce    sync.Once
}

// NewSine creates the estimator and starts updating the pacer.
func NewSine(p SineParams) *Sine {
	s := newSine(p, time.Now, gcc.NewLeakyBucketPacer(p.CenterBps))
	go s.loop()
	return s
}

func newSine(p SineParams, now func() time.Time, pacer gcc.Pacer) *Sine {
	return &Sine{p: p, now: now, pacer: pacer, done: make(chan struct{})}
}

// Start sets t=0 of the wave. Before Start the target is the center.
func (s *Sine) Start() {
	s.mu.Lock()
	if s.t0.IsZero() {
		s.t0 = s.now()
	}
	s.mu.Unlock()
}

func (s *Sine) elapsed() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.t0.IsZero() {
		return 0
	}
	return s.now().Sub(s.t0)
}

func (s *Sine) loop() {
	t := time.NewTicker(pacerUpdateInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		rate := s.GetTargetBitrate()
		s.pacer.SetTargetBitrate(rate)
		s.mu.Lock()
		f := s.onChange
		s.mu.Unlock()
		if f != nil {
			f(rate)
		}
	}
}

// AddStream routes the stream through the pacer.
func (s *Sine) AddStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	s.pacer.AddStream(info.SSRC, writer)
	return s.pacer
}

// WriteRTCP counts feedback; it never changes the rate.
func (s *Sine) WriteRTCP(pkts []rtcp.Packet, _ interceptor.Attributes) error {
	for _, pkt := range pkts {
		s.rtcpPackets.Add(1)
		switch pkt.(type) {
		case *rtcp.TransportLayerCC, *rtcp.CCFeedbackReport:
			s.twccFeedback.Add(1)
		}
	}
	return nil
}

// GetTargetBitrate returns the wave's current value (bps, all streams).
func (s *Sine) GetTargetBitrate() int {
	return s.p.RateAt(s.elapsed())
}

// OnTargetBitrateChange registers a callback run on every pacer update.
func (s *Sine) OnTargetBitrateChange(f func(int)) {
	s.mu.Lock()
	s.onChange = f
	s.mu.Unlock()
}

// GetStats reports the wave parameters and position.
func (s *Sine) GetStats() map[string]any {
	el := s.elapsed()
	period := s.p.Period.Seconds()
	return map[string]any{
		"algorithm":           "sine",
		"centerBps":           s.p.CenterBps,
		"amplitudeBps":        s.p.AmplitudeBps,
		"periodSec":           period,
		"elapsedSec":          el.Seconds(),
		"phase":               math.Mod(el.Seconds(), period) / period, // 0..1 through the current cycle
		"twccFeedbackPackets": s.twccFeedback.Load(),
		"rtcpPackets":         s.rtcpPackets.Load(),
	}
}

// Close stops the update loop and the pacer.
func (s *Sine) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.done)
		err = s.pacer.Close()
	})
	return err
}
