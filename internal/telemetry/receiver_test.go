package telemetry

import (
	"math"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func TestH264FrameStartAndKeyframe(t *testing.T) {
	stapA := []byte{24, 0, 2, 0x67, 0x42, 0, 2, 0x68, 0xce} // SPS + PPS
	fuStartIDR := []byte{28, 0x85, 0x88}                    // FU-A start, IDR, first_mb=0
	fuMidIDR := []byte{28, 0x05, 0x00}                      // FU-A continuation
	sliceSecond := []byte{0x41, 0x40}                       // non-IDR slice, first_mb != 0
	for _, c := range []struct {
		name       string
		b          []byte
		start, key bool
	}{
		{"stap-a sps/pps", stapA, true, true},
		{"fu-a idr start", fuStartIDR, true, true},
		{"fu-a idr middle", fuMidIDR, false, false},
		{"second slice", sliceSecond, false, false},
	} {
		if got := h264FrameStart(c.b); got != c.start {
			t.Errorf("%s: start=%v want %v", c.name, got, c.start)
		}
		if got := h264Keyframe(c.b); got != c.key {
			t.Errorf("%s: key=%v want %v", c.name, got, c.key)
		}
	}
}

func TestSeqTrackerWrapAndLoss(t *testing.T) {
	var tr seqTracker
	for _, s := range []uint16{65533, 65534, 65535, 1, 2, 0} { // 0 arrives late
		tr.observe(s)
	}
	if tr.expected() != 6 || tr.lost() != 0 || tr.reorders != 1 {
		t.Fatalf("expected=%d lost=%d reorders=%d", tr.expected(), tr.lost(), tr.reorders)
	}
	tr.observe(5) // 3, 4 missing
	if tr.lost() != 2 {
		t.Fatalf("lost=%d want 2", tr.lost())
	}
}

// Two-packet H264 frames at 30 fps; drop one packet of frame 5 and check the
// frame is reported lost, later deltas wait for a keyframe, and the first
// frame is not dropped.
func TestVideoReceiverFrames(t *testing.T) {
	s := NewVideoReceiverStats("h264", 90000)
	now := time.Now()
	seq := uint16(100)
	send := func(frame int, key, dropSecond bool) {
		ts := uint32(1000 + frame*3000)
		first := []byte{28, 0x81, 0x80} // FU-A start, non-IDR
		last := []byte{28, 0x41, 0x00}  // FU-A end
		if key {
			first, last = []byte{28, 0x85, 0x88}, []byte{28, 0x45, 0x00}
		}
		s.Packet(&rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: ts}, Payload: first}, 100, now, Clock{math.NaN(), math.NaN()})
		seq++
		if !dropSecond {
			s.Packet(&rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: ts, Marker: true}, Payload: last}, 100, now, Clock{math.NaN(), math.NaN()})
		}
		seq++
		now = now.Add(33 * time.Millisecond)
	}
	send(0, true, false)
	for i := 1; i < 5; i++ {
		send(i, false, false)
	}
	if s.frames != 5 || s.lostFrames != 0 {
		t.Fatalf("frames=%d lost=%d after clean start", s.frames, s.lostFrames)
	}
	send(5, false, true)
	for i := 6; i < 20; i++ { // >300 ms of newer complete frames
		send(i, false, false)
	}
	if s.lostFrames == 0 || !s.needKeyframe {
		t.Fatalf("lost=%d needKeyframe=%v, want loss detected", s.lostFrames, s.needKeyframe)
	}
	before := s.frames
	send(20, true, false)
	send(21, false, false)
	if s.frames != before+2 || s.needKeyframe {
		t.Fatalf("frames=%d (before %d) needKeyframe=%v after keyframe", s.frames, before, s.needKeyframe)
	}
}
