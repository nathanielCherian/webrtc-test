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

// Wave shapes.
const (
	ShapeSine   = "sine"
	ShapeSquare = "square"
)

// WaveParams describes a periodic rate around Center:
//   - sine:   Center + Amplitude*sin(2*pi*t/Period)
//   - square: Center + Amplitude for the first Duty of each period, then
//     Center - Amplitude. Amplitude < Center keeps the low phase above zero.
type WaveParams struct {
	Shape        string        `json:"shape"`
	CenterBps    int           `json:"centerBps"`
	AmplitudeBps int           `json:"amplitudeBps"`
	Period       time.Duration `json:"period"`
	Duty         float64       `json:"duty,omitempty"` // square only: fraction of the period at the high rate
}

// Validate checks that the rate stays positive and below maxBps.
func (p WaveParams) Validate(maxBps int) error {
	switch {
	case p.Shape != ShapeSine && p.Shape != ShapeSquare:
		return fmt.Errorf("unknown wave shape %q", p.Shape)
	case p.CenterBps <= 0:
		return errors.New("center must be > 0")
	case p.AmplitudeBps < 0 || p.AmplitudeBps >= p.CenterBps:
		return errors.New("amplitude must be >= 0 and < center")
	case p.Period < time.Second:
		return errors.New("period must be >= 1 s")
	case p.Shape == ShapeSquare && (p.Duty <= 0 || p.Duty >= 1):
		return errors.New("duty cycle must be between 0 and 1 (exclusive)")
	case maxBps > 0 && p.CenterBps+p.AmplitudeBps > maxBps:
		return fmt.Errorf("peak %d bps exceeds --max-bitrate %d", p.CenterBps+p.AmplitudeBps, maxBps)
	}
	return nil
}

// phase returns the position within the current cycle, in [0, 1).
func (p WaveParams) phase(elapsed time.Duration) float64 {
	f := math.Mod(elapsed.Seconds()/p.Period.Seconds(), 1)
	if f < 0 {
		f++
	}
	return f
}

// RateAt returns the rate elapsed into the wave.
func (p WaveParams) RateAt(elapsed time.Duration) int {
	if p.Shape == ShapeSquare {
		if p.phase(elapsed) < p.Duty {
			return p.CenterBps + p.AmplitudeBps
		}
		return p.CenterBps - p.AmplitudeBps
	}
	return p.CenterBps + int(math.Round(float64(p.AmplitudeBps)*math.Sin(2*math.Pi*p.phase(elapsed))))
}

// pacerUpdateInterval is how often the pacer rate follows the wave.
const pacerUpdateInterval = 20 * time.Millisecond

// Wave is a cc.BandwidthEstimator whose target follows a fixed periodic
// waveform and ignores feedback. Packets go through the same leaky bucket pacer GCC uses.
type Wave struct {
	p     WaveParams
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

// NewWave creates the estimator and starts updating the pacer.
func NewWave(p WaveParams) *Wave {
	s := newWave(p, time.Now, gcc.NewLeakyBucketPacer(p.CenterBps))
	go s.loop()
	return s
}

func newWave(p WaveParams, now func() time.Time, pacer gcc.Pacer) *Wave {
	return &Wave{p: p, now: now, pacer: pacer, done: make(chan struct{})}
}

// Start sets t=0 of the wave. Before Start the target is the center.
func (s *Wave) Start() {
	s.mu.Lock()
	if s.t0.IsZero() {
		s.t0 = s.now()
	}
	s.mu.Unlock()
}

func (s *Wave) elapsed() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.t0.IsZero() {
		return 0
	}
	return s.now().Sub(s.t0)
}

func (s *Wave) loop() {
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
func (s *Wave) AddStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	s.pacer.AddStream(info.SSRC, writer)
	return s.pacer
}

// WriteRTCP counts feedback; it never changes the rate.
func (s *Wave) WriteRTCP(pkts []rtcp.Packet, _ interceptor.Attributes) error {
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
func (s *Wave) GetTargetBitrate() int {
	return s.p.RateAt(s.elapsed())
}

// OnTargetBitrateChange registers a callback run on every pacer update.
func (s *Wave) OnTargetBitrateChange(f func(int)) {
	s.mu.Lock()
	s.onChange = f
	s.mu.Unlock()
}

// GetStats reports the wave parameters and position.
func (s *Wave) GetStats() map[string]any {
	el := s.elapsed()
	st := map[string]any{
		"algorithm":           s.p.Shape,
		"centerBps":           s.p.CenterBps,
		"amplitudeBps":        s.p.AmplitudeBps,
		"periodSec":           s.p.Period.Seconds(),
		"elapsedSec":          el.Seconds(),
		"phase":               s.p.phase(el), // 0..1 through the current cycle
		"twccFeedbackPackets": s.twccFeedback.Load(),
		"rtcpPackets":         s.rtcpPackets.Load(),
	}
	if s.p.Shape == ShapeSquare {
		st["duty"] = s.p.Duty
	}
	return st
}

// Close stops the update loop and the pacer.
func (s *Wave) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.done)
		err = s.pacer.Close()
	})
	return err
}
