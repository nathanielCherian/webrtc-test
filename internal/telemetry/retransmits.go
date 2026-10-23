package telemetry

import (
	"sync"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

// Retransmits counts the RTP packets the NACK responder resends. It is an
// interceptor factory for one peer connection: register it after the stats
// interceptor and before ConfigureNack, so it sits between the responder and
// the wire and sees both originals and resends.
//
// Without RTX a resend reuses the original SSRC and sequence number, so a
// packet whose sequence number is not newer than the highest one sent on its
// SSRC is counted as a resend. The session writes each SSRC in order, so
// nothing else goes backwards.
type Retransmits struct {
	interceptor.NoOp
	mu      sync.Mutex
	streams map[uint32]*retransmitStream
}

type retransmitStream struct {
	started        bool
	highestSeq     uint16
	packets, bytes int64
}

// NewRetransmits creates an empty counter.
func NewRetransmits() *Retransmits {
	return &Retransmits{streams: map[uint32]*retransmitStream{}}
}

// NewInterceptor implements interceptor.Factory.
func (r *Retransmits) NewInterceptor(string) (interceptor.Interceptor, error) { return r, nil }

// BindLocalStream wraps the stream's writer to spot resends.
func (r *Retransmits) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	r.mu.Lock()
	st := &retransmitStream{}
	r.streams[info.SSRC] = st
	r.mu.Unlock()
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
		r.mu.Lock()
		switch {
		case !st.started:
			st.started, st.highestSeq = true, header.SequenceNumber
		case int16(header.SequenceNumber-st.highestSeq) > 0:
			st.highestSeq = header.SequenceNumber
		default:
			st.packets++
			st.bytes += int64(header.MarshalSize() + len(payload))
		}
		r.mu.Unlock()
		return writer.Write(header, payload, attributes)
	})
}

// Snapshot returns the resend counts for ssrc as a JSON-friendly map.
func (r *Retransmits) Snapshot(ssrc uint32) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.streams[ssrc]
	if st == nil {
		return map[string]any{"packets": 0, "bytes": 0}
	}
	return map[string]any{"packets": st.packets, "bytes": st.bytes}
}
