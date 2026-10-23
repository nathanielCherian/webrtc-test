package main

import (
	"context"
	"encoding/binary"
	"errors"
	"log"
	"math"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
)

// Datagram wire format (all integers big-endian). Server times are Unix
// microseconds from the server's wall clock; the client's t1 is echoed as-is.
//
//	beacon  server→client  [0]=1  [1:5] seq uint32  [5:13] tSend int64  [13:] zero padding
//	ping    client→server  [0]=2  [1:5] id uint32   [5:13] t1 float64 (client ms)
//	pong    server→client  [0]=3  [1:5] id uint32   [5:13] t1 float64 (echoed)
//	                              [13:21] t2 int64 (ping received)  [21:29] t3 int64 (pong sent)
const (
	typeBeacon = 1
	typePing   = 2
	typePong   = 3

	beaconHeaderLen = 13
	pingLen         = 13
	pongLen         = 29

	// Comfortably below the smallest QUIC datagram frame a 1280-byte path allows.
	maxDatagramSize = 1100
)

func appendBeacon(b []byte, seq uint32, tSend time.Time, size int) []byte {
	b = append(b, typeBeacon)
	b = binary.BigEndian.AppendUint32(b, seq)
	b = binary.BigEndian.AppendUint64(b, uint64(tSend.UnixMicro()))
	for len(b) < size {
		b = append(b, 0)
	}
	return b
}

// pongFor answers a ping datagram, or returns nil if p isn't one.
func pongFor(p []byte, received, now time.Time) []byte {
	if len(p) < pingLen || p[0] != typePing {
		return nil
	}
	b := make([]byte, 0, pongLen)
	b = append(b, typePong)
	b = append(b, p[1:13]...) // id and t1, echoed
	b = binary.BigEndian.AppendUint64(b, uint64(received.UnixMicro()))
	b = binary.BigEndian.AppendUint64(b, uint64(now.UnixMicro()))
	return b
}

type beacon struct {
	wt          *webtransport.Server
	interval    time.Duration // default when the client doesn't ask for one
	maxSessions int64
	active      atomic.Int64
}

// sessionParams reads ?interval=<ms>&size=<bytes> from the session URL.
func (b *beacon) sessionParams(r *http.Request) (time.Duration, int) {
	interval, size := b.interval, beaconHeaderLen
	q := r.URL.Query()
	if v, err := strconv.ParseFloat(q.Get("interval"), 64); err == nil && !math.IsNaN(v) {
		interval = time.Duration(min(max(v, 1), 1000) * float64(time.Millisecond))
	}
	if v, err := strconv.Atoi(q.Get("size")); err == nil {
		size = min(max(v, beaconHeaderLen), maxDatagramSize)
	}
	return interval, size
}

func (b *beacon) handle(w http.ResponseWriter, r *http.Request) {
	if b.active.Add(1) > b.maxSessions {
		b.active.Add(-1)
		http.Error(w, "too many sessions", http.StatusServiceUnavailable)
		return
	}
	interval, size := b.sessionParams(r)
	sess, err := b.wt.Upgrade(w, r)
	if err != nil {
		b.active.Add(-1)
		log.Printf("upgrade from %s: %v", r.RemoteAddr, err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// The session outlives this handler (Upgrade takes over the stream).
	go func() {
		defer b.active.Add(-1)
		start := time.Now()
		log.Printf("session %s: interval %v, %d-byte beacons (%d active)", sess.RemoteAddr(), interval, size, b.active.Load())
		go answerPings(sess)
		sent, err := sendBeacons(sess, interval, size)
		log.Printf("session %s closed after %v, %d beacons: %v", sess.RemoteAddr(), time.Since(start).Round(time.Second), sent, err)
	}()
}

// answerPings echoes every ping with the server's receive and send times.
func answerPings(sess *webtransport.Session) {
	for {
		p, err := sess.ReceiveDatagram(sess.Context())
		if err != nil {
			return
		}
		received := time.Now()
		if pong := pongFor(p, received, time.Now()); pong != nil {
			_ = sess.SendDatagram(pong)
		}
	}
}

// sendBeacons sends a timestamped datagram every interval until the session ends.
// tSend is taken before the datagram is queued, so time spent waiting for QUIC
// congestion control counts toward the delay. SendDatagram blocks while the
// queue is full; the ticks missed meanwhile are dropped, not made up.
func sendBeacons(sess *webtransport.Session, interval time.Duration, size int) (uint32, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	buf := make([]byte, 0, maxDatagramSize)
	var seq uint32
	for {
		select {
		case <-sess.Context().Done():
			return seq, context.Cause(sess.Context())
		case <-ticker.C:
		}
		err := sess.SendDatagram(appendBeacon(buf[:0], seq, time.Now(), size))
		var tooLarge *quic.DatagramTooLargeError
		if errors.As(err, &tooLarge) && size > beaconHeaderLen {
			// The path allows less than asked; shrink (leaving room for the
			// session ID prefix) and send this seq again on the next tick.
			size = max(beaconHeaderLen, int(tooLarge.MaxDatagramPayloadSize)-8)
			log.Printf("session %s: beacons shrunk to %d bytes", sess.RemoteAddr(), size)
			continue
		}
		if err != nil {
			return seq, err
		}
		seq++
	}
}
