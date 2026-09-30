package telemetry

import (
	"sort"
	"sync"
	"time"
)

// EncoderStats accumulates per-frame encoder measurements. Window() returns
// rates over the interval since the previous Window() call plus cumulative
// counters.
type EncoderStats struct {
	mu sync.Mutex

	// cumulative
	framesEncoded   int64
	bytesEncoded    int64
	keyframes       int64
	keyframesForced int64 // IDRs requested because of PLI/FIR
	framesSkipped   int64 // encoder produced no output
	bitrateChanges  int64
	lastFrameBytes  int
	lastKeyframeAt  time.Time
	configuredBps   int
	targetBps       int // last GCC target before headroom

	// window
	winStart     time.Time
	winFrames    int64
	winBytes     int64
	winEncodeDur []float64 // ms
	winSendDur   []float64 // ms, WriteSample duration (includes pacer queueing)
	winMaxFrame  int
}

// NewEncoderStats creates an accumulator.
func NewEncoderStats() *EncoderStats {
	return &EncoderStats{winStart: time.Now()}
}

// Frame records one encoded frame.
func (s *EncoderStats) Frame(bytes int, key, forced bool, encodeDur, sendDur time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.framesEncoded++
	s.bytesEncoded += int64(bytes)
	s.lastFrameBytes = bytes
	if key {
		s.keyframes++
		s.lastKeyframeAt = time.Now()
		if forced {
			s.keyframesForced++
		}
	}
	s.winFrames++
	s.winBytes += int64(bytes)
	if bytes > s.winMaxFrame {
		s.winMaxFrame = bytes
	}
	s.winEncodeDur = append(s.winEncodeDur, float64(encodeDur.Microseconds())/1000)
	s.winSendDur = append(s.winSendDur, float64(sendDur.Microseconds())/1000)
}

// Skipped records an input frame that produced no encoder output.
func (s *EncoderStats) Skipped() {
	s.mu.Lock()
	s.framesSkipped++
	s.mu.Unlock()
}

// Bitrate records a bitrate reconfiguration.
func (s *EncoderStats) Bitrate(targetBps, configuredBps int) {
	s.mu.Lock()
	s.bitrateChanges++
	s.targetBps = targetBps
	s.configuredBps = configuredBps
	s.mu.Unlock()
}

// Window returns a snapshot and resets the windowed counters.
func (s *EncoderStats) Window() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	secs := now.Sub(s.winStart).Seconds()
	if secs <= 0 {
		secs = 1e-9
	}
	out := map[string]any{
		"framesEncoded":         s.framesEncoded,
		"bytesEncoded":          s.bytesEncoded,
		"keyframes":             s.keyframes,
		"keyframesForced":       s.keyframesForced,
		"framesSkipped":         s.framesSkipped,
		"bitrateChanges":        s.bitrateChanges,
		"configuredBitrate":     s.configuredBps,
		"gccTargetBitrate":      s.targetBps,
		"lastFrameBytes":        s.lastFrameBytes,
		"windowSec":             secs,
		"outputBitrate":         float64(s.winBytes*8) / secs,
		"outputFps":             float64(s.winFrames) / secs,
		"maxFrameBytes":         s.winMaxFrame,
		"encodeMsMean":          mean(s.winEncodeDur),
		"encodeMsP95":           pct(s.winEncodeDur, 0.95),
		"encodeMsMax":           pct(s.winEncodeDur, 1),
		"writeSampleMsMean":     mean(s.winSendDur),
		"writeSampleMsP95":      pct(s.winSendDur, 0.95),
		"msSinceLastKeyframe":   sinceMs(now, s.lastKeyframeAt),
		"avgFrameBytesInWindow": 0.0,
	}
	if s.winFrames > 0 {
		out["avgFrameBytesInWindow"] = float64(s.winBytes) / float64(s.winFrames)
	}
	s.winStart = now
	s.winFrames = 0
	s.winBytes = 0
	s.winMaxFrame = 0
	s.winEncodeDur = s.winEncodeDur[:0]
	s.winSendDur = s.winSendDur[:0]
	return out
}

func sinceMs(now, t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return float64(now.Sub(t).Microseconds()) / 1000
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sum := 0.0
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}

func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	i := int(p * float64(len(c)-1))
	return c[i]
}
