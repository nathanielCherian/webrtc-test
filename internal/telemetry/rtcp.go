package telemetry

import (
	"sync"
	"time"

	"github.com/pion/rtcp"
)

// RTCPCounters tallies the RTCP feedback a sender receives from the browser.
type RTCPCounters struct {
	mu   sync.Mutex
	ssrc uint32

	PLI, FIR, NackPackets, NackedSeqs, RR, SR, REMB, TWCC, Other int64
	lastREMB                                                     float32
	lastRR                                                       *rtcp.ReceptionReport
	lastRRAt                                                     time.Time
	lastPLIAt                                                    time.Time
	lastRTT                                                      time.Duration
	rttSamples                                                   int64
}

// NewRTCPCounters creates counters for the given local media SSRC.
func NewRTCPCounters(ssrc uint32) *RTCPCounters { return &RTCPCounters{ssrc: ssrc} }

// Observe records a batch of RTCP packets. It reports whether a keyframe was requested.
func (c *RTCPCounters) Observe(pkts []rtcp.Packet) (keyframeRequested bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range pkts {
		switch p := p.(type) {
		case *rtcp.PictureLossIndication:
			c.PLI++
			c.lastPLIAt = time.Now()
			keyframeRequested = true
		case *rtcp.FullIntraRequest:
			c.FIR++
			keyframeRequested = true
		case *rtcp.TransportLayerNack:
			c.NackPackets++
			for _, pair := range p.Nacks {
				c.NackedSeqs += int64(len(pair.PacketList()))
			}
		case *rtcp.ReceiverReport:
			c.RR++
			c.observeReports(p.Reports)
		case *rtcp.SenderReport:
			c.SR++
			c.observeReports(p.Reports)
		case *rtcp.ReceiverEstimatedMaximumBitrate:
			c.REMB++
			c.lastREMB = p.Bitrate
		case *rtcp.TransportLayerCC:
			c.TWCC++
		default:
			c.Other++
		}
	}
	return keyframeRequested
}

func (c *RTCPCounters) observeReports(reports []rtcp.ReceptionReport) {
	for i := range reports {
		if reports[i].SSRC == c.ssrc {
			r := reports[i]
			c.lastRR = &r
			c.lastRRAt = time.Now()
			if r.LastSenderReport != 0 {
				// RFC 3550 6.4.1: RTT = now - LSR - DLSR, in 1/65536 s units
				// (compact NTP). Assumes the SR was sent by this host's clock.
				rtt := ntpCompact(c.lastRRAt) - r.LastSenderReport - r.Delay
				if rtt < 1<<31 { // ignore wrap/negative
					c.lastRTT = time.Duration(float64(rtt) / 65536 * float64(time.Second))
					c.rttSamples++
				}
			}
		}
	}
}

func ntpCompact(t time.Time) uint32 {
	const ntpEpochOffset = 2208988800
	secs := uint64(t.Unix()) + ntpEpochOffset
	frac := uint64(t.Nanosecond()) << 32 / 1e9
	return uint32((secs<<32 | frac) >> 16)
}

// Snapshot returns the counters as a JSON-friendly map.
func (c *RTCPCounters) Snapshot() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]any{
		"ssrc":         c.ssrc,
		"pli":          c.PLI,
		"fir":          c.FIR,
		"nackPackets":  c.NackPackets,
		"nackedSeqs":   c.NackedSeqs,
		"rr":           c.RR,
		"sr":           c.SR,
		"remb":         c.REMB,
		"twccFeedback": c.TWCC,
		"other":        c.Other,
	}
	if c.rttSamples > 0 {
		out["rttMs"] = float64(c.lastRTT.Microseconds()) / 1000
		out["rttSamples"] = c.rttSamples
	}
	if c.REMB > 0 {
		out["lastRembBps"] = c.lastREMB
	}
	if c.lastRR != nil {
		out["lastReport"] = map[string]any{
			"fractionLost":     float64(c.lastRR.FractionLost) / 256,
			"totalLost":        c.lastRR.TotalLost,
			"highestSeq":       c.lastRR.LastSequenceNumber,
			"jitterRtpUnits":   c.lastRR.Jitter,
			"lastSR":           c.lastRR.LastSenderReport,
			"delaySinceLastSR": c.lastRR.Delay,
			"msSinceReceived":  float64(time.Since(c.lastRRAt).Microseconds()) / 1000,
		}
	}
	return out
}
