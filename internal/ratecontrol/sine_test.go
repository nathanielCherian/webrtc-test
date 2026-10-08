package ratecontrol

import (
	"testing"
	"time"

	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/rtcp"
)

func TestSineRate(t *testing.T) {
	p := SineParams{CenterBps: 1_000_000, AmplitudeBps: 500_000, Period: 8 * time.Second}
	now := time.Unix(1000, 0)
	s := newSine(p, func() time.Time { return now }, gcc.NewNoOpPacer())

	if got := s.GetTargetBitrate(); got != p.CenterBps {
		t.Fatalf("before Start: got %d, want center", got)
	}
	s.Start()
	for _, c := range []struct {
		at   time.Duration
		want int
	}{
		{0, 1_000_000},
		{2 * time.Second, 1_500_000},
		{4 * time.Second, 1_000_000},
		{6 * time.Second, 500_000},
		{8 * time.Second, 1_000_000},
		{10 * time.Second, 1_500_000},
	} {
		now = time.Unix(1000, 0).Add(c.at)
		if got := s.GetTargetBitrate(); got != c.want {
			t.Errorf("t=%v: got %d, want %d", c.at, got, c.want)
		}
	}
	if err := s.WriteRTCP([]rtcp.Packet{&rtcp.TransportLayerCC{}, &rtcp.ReceiverReport{}}, nil); err != nil {
		t.Fatal(err)
	}
	st := s.GetStats()
	if st["twccFeedbackPackets"].(int64) != 1 || st["rtcpPackets"].(int64) != 2 {
		t.Errorf("feedback counters: %v", st)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSineValidate(t *testing.T) {
	ok := SineParams{CenterBps: 1_000_000, AmplitudeBps: 500_000, Period: 10 * time.Second}
	if err := ok.Validate(6_000_000); err != nil {
		t.Fatalf("valid params rejected: %v", err)
	}
	for name, p := range map[string]SineParams{
		"zero center":   {CenterBps: 0, AmplitudeBps: 0, Period: time.Second},
		"amp >= center": {CenterBps: 1_000_000, AmplitudeBps: 1_000_000, Period: time.Second},
		"negative amp":  {CenterBps: 1_000_000, AmplitudeBps: -1, Period: time.Second},
		"short period":  {CenterBps: 1_000_000, AmplitudeBps: 0, Period: 500 * time.Millisecond},
		"peak over cap": {CenterBps: 5_000_000, AmplitudeBps: 2_000_000, Period: time.Second},
	} {
		if err := p.Validate(6_000_000); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
