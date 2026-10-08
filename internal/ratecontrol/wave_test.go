package ratecontrol

import (
	"testing"
	"time"

	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/rtcp"
)

type point struct {
	at   time.Duration
	want int
}

func checkWave(t *testing.T, p WaveParams, points []point) *Wave {
	t.Helper()
	now := time.Unix(1000, 0)
	w := newWave(p, func() time.Time { return now }, gcc.NewNoOpPacer())
	if got := w.GetTargetBitrate(); got != p.RateAt(0) {
		t.Fatalf("before Start: got %d, want %d", got, p.RateAt(0))
	}
	w.Start()
	for _, c := range points {
		now = time.Unix(1000, 0).Add(c.at)
		if got := w.GetTargetBitrate(); got != c.want {
			t.Errorf("%s t=%v: got %d, want %d", p.Shape, c.at, got, c.want)
		}
	}
	return w
}

func TestSineRate(t *testing.T) {
	p := WaveParams{Shape: ShapeSine, CenterBps: 1_000_000, AmplitudeBps: 500_000, Period: 8 * time.Second}
	w := checkWave(t, p, []point{
		{0, 1_000_000},
		{2 * time.Second, 1_500_000},
		{4 * time.Second, 1_000_000},
		{6 * time.Second, 500_000},
		{8 * time.Second, 1_000_000},
		{10 * time.Second, 1_500_000},
	})
	if err := w.WriteRTCP([]rtcp.Packet{&rtcp.TransportLayerCC{}, &rtcp.ReceiverReport{}}, nil); err != nil {
		t.Fatal(err)
	}
	st := w.GetStats()
	if st["algorithm"] != ShapeSine || st["twccFeedbackPackets"].(int64) != 1 || st["rtcpPackets"].(int64) != 2 {
		t.Errorf("stats: %v", st)
	}
	if _, ok := st["duty"]; ok {
		t.Error("sine stats should not report a duty cycle")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSquareRate(t *testing.T) {
	// 10 s period, high for the first 30%.
	p := WaveParams{Shape: ShapeSquare, CenterBps: 1_000_000, AmplitudeBps: 600_000, Period: 10 * time.Second, Duty: 0.3}
	w := checkWave(t, p, []point{
		{0, 1_600_000},
		{2900 * time.Millisecond, 1_600_000},
		{3 * time.Second, 400_000},
		{9900 * time.Millisecond, 400_000},
		{10 * time.Second, 1_600_000},
		{13500 * time.Millisecond, 400_000},
	})
	if st := w.GetStats(); st["algorithm"] != ShapeSquare || st["duty"] != 0.3 {
		t.Errorf("stats: %v", st)
	}
	_ = w.Close()
}

func TestWaveValidate(t *testing.T) {
	for _, ok := range []WaveParams{
		{Shape: ShapeSine, CenterBps: 1_000_000, AmplitudeBps: 500_000, Period: 10 * time.Second},
		{Shape: ShapeSquare, CenterBps: 1_000_000, AmplitudeBps: 500_000, Period: 10 * time.Second, Duty: 0.5},
	} {
		if err := ok.Validate(6_000_000); err != nil {
			t.Errorf("valid %s params rejected: %v", ok.Shape, err)
		}
	}
	for name, p := range map[string]WaveParams{
		"unknown shape": {Shape: "triangle", CenterBps: 1_000_000, Period: time.Second},
		"zero center":   {Shape: ShapeSine, CenterBps: 0, AmplitudeBps: 0, Period: time.Second},
		"amp >= center": {Shape: ShapeSquare, CenterBps: 1_000_000, AmplitudeBps: 1_000_000, Period: time.Second, Duty: 0.5},
		"negative amp":  {Shape: ShapeSine, CenterBps: 1_000_000, AmplitudeBps: -1, Period: time.Second},
		"short period":  {Shape: ShapeSine, CenterBps: 1_000_000, AmplitudeBps: 0, Period: 500 * time.Millisecond},
		"peak over cap": {Shape: ShapeSine, CenterBps: 5_000_000, AmplitudeBps: 2_000_000, Period: time.Second},
		"duty zero":     {Shape: ShapeSquare, CenterBps: 1_000_000, AmplitudeBps: 0, Period: time.Second, Duty: 0},
		"duty one":      {Shape: ShapeSquare, CenterBps: 1_000_000, AmplitudeBps: 0, Period: time.Second, Duty: 1},
	} {
		if err := p.Validate(6_000_000); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
