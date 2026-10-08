package ratecontrol

import (
	"strings"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/rtp"
)

// Padding probes.
//
// A sender that forwards fixed-rate simulcast layers can't raise its own rate to
// test the network, and Pion's GCC never lets its estimate grow past 1.5x the
// rate it sees acknowledged. So a viewer parked on a low layer would never find
// out that a higher one fits. The fix, as in libwebrtc, is to send padding up to
// the estimate.
//
// RTP padding-only packets are the right thing on the wire (receivers discard
// them), but Pion's GCC sizes packets by len(payload) and would count them as
// empty. So probes are written as an ordinary payload of zero bytes (no real H.264
// RTP payload starts with 0x00, whose NAL type 0 is unused), counted by GCC like
// media, and turned into padding-only packets just after GCC, before SRTP.

// MaxProbeBytes is the largest probe (RTP padding length is one byte).
const MaxProbeBytes = 255

var zeroProbe = make([]byte, MaxProbeBytes)

// ProbePayload returns an n-byte probe payload (1..MaxProbeBytes).
func ProbePayload(n int) []byte {
	return zeroProbe[:max(1, min(n, MaxProbeBytes))]
}

// IsProbePayload reports whether payload is a probe written with ProbePayload.
func IsProbePayload(payload []byte) bool {
	return len(payload) > 0 && len(payload) <= MaxProbeBytes && payload[0] == 0
}

// PaddingProbes wraps a bandwidth estimator so probe payloads count towards its
// rate measurements and leave as RTP padding.
type PaddingProbes struct {
	cc.BandwidthEstimator
}

// WithPaddingProbes wraps e.
func WithPaddingProbes(e cc.BandwidthEstimator) *PaddingProbes { return &PaddingProbes{e} }

// Unwrap returns the wrapped estimator.
func (p *PaddingProbes) Unwrap() cc.BandwidthEstimator { return p.BandwidthEstimator }

// AddStream converts probes after the estimator has seen them. Only H.264
// streams carry probes (an Opus payload may legitimately start with 0x00).
func (p *PaddingProbes) AddStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	if !strings.EqualFold(info.MimeType, "video/H264") {
		return p.BandwidthEstimator.AddStream(info, writer)
	}
	return p.BandwidthEstimator.AddStream(info, interceptor.RTPWriterFunc(
		func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
			if !IsProbePayload(payload) {
				return writer.Write(header, payload, attributes)
			}
			h := *header
			h.Padding = true
			h.PaddingSize = byte(len(payload))
			return writer.Write(&h, nil, attributes)
		}))
}
