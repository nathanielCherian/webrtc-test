package telemetry

import (
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// Registered as in the session (counter before the NACK responder), resends of
// NACKed packets are counted and originals are not.
func TestRetransmitsCountsNackResends(t *testing.T) {
	const ssrc = 1234
	counter := NewRetransmits()
	responder, err := nack.NewResponderInterceptor()
	if err != nil {
		t.Fatal(err)
	}
	ir := &interceptor.Registry{}
	ir.Add(counter)
	ir.Add(responder)
	chain, err := ir.Build("")
	if err != nil {
		t.Fatal(err)
	}
	defer chain.Close()

	sent := make(chan uint16, 64)
	info := &interceptor.StreamInfo{SSRC: ssrc, RTCPFeedback: []interceptor.RTCPFeedback{{Type: "nack"}}}
	w := chain.BindLocalStream(info, interceptor.RTPWriterFunc(
		func(h *rtp.Header, _ []byte, _ interceptor.Attributes) (int, error) {
			sent <- h.SequenceNumber
			return 0, nil
		}))
	// Start just below the wrap so it is covered too.
	for seq := uint16(65530); seq != 6; seq++ {
		if _, err := w.Write(&rtp.Header{SSRC: ssrc, SequenceNumber: seq}, make([]byte, 100), nil); err != nil {
			t.Fatal(err)
		}
		<-sent
	}
	if got := counter.Snapshot(ssrc)["packets"]; got != int64(0) {
		t.Fatalf("resends before any NACK = %v, want 0", got)
	}

	// NACK 65534 and 0..2 (PacketID 0, bitmask 0b11).
	raw, err := (&rtcp.TransportLayerNack{MediaSSRC: ssrc, Nacks: []rtcp.NackPair{
		{PacketID: 65534}, {PacketID: 0, LostPackets: 0b11},
	}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	r := chain.BindRTCPReader(interceptor.RTCPReaderFunc(
		func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
			return copy(b, raw), a, nil
		}))
	if _, _, err := r.Read(make([]byte, 1500), nil); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		select {
		case <-sent:
		case <-time.After(time.Second):
			t.Fatal("NACKed packet not resent")
		}
	}
	snap := counter.Snapshot(ssrc)
	if snap["packets"] != int64(4) {
		t.Errorf("resent packets = %v, want 4", snap["packets"])
	}
	if want := int64(4 * (12 + 100)); snap["bytes"] != want {
		t.Errorf("resent bytes = %v, want %v", snap["bytes"], want)
	}
}
