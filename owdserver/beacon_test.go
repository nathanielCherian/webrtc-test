package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

func TestPongFor(t *testing.T) {
	ping := []byte{typePing}
	ping = binary.BigEndian.AppendUint32(ping, 7)
	ping = binary.BigEndian.AppendUint64(ping, math.Float64bits(1234.5))
	recv := time.UnixMicro(1_000_000)
	sent := time.UnixMicro(1_000_250)
	p := pongFor(ping, recv, sent)
	if len(p) != pongLen || p[0] != typePong {
		t.Fatalf("pong = %v", p)
	}
	if id := binary.BigEndian.Uint32(p[1:]); id != 7 {
		t.Errorf("id = %d", id)
	}
	if t1 := math.Float64frombits(binary.BigEndian.Uint64(p[5:])); t1 != 1234.5 {
		t.Errorf("t1 = %v", t1)
	}
	if t2, t3 := int64(binary.BigEndian.Uint64(p[13:])), int64(binary.BigEndian.Uint64(p[21:])); t2 != 1_000_000 || t3 != 1_000_250 {
		t.Errorf("t2, t3 = %d, %d", t2, t3)
	}
	if pongFor([]byte{typeBeacon, 0, 0}, recv, sent) != nil {
		t.Error("answered a non-ping")
	}
}

// Dial the beacon like a browser does (pinning the self-signed hash) and check
// that beacons arrive in order with sane timestamps and that pings are answered.
func TestSession(t *testing.T) {
	certs, err := newSelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := newBeacon(certs, 10*time.Millisecond, 4)
	go b.wt.Serve(udp)
	t.Cleanup(func() { b.wt.Close() })

	pinned := certs.info()["hashes"].([]string)[0]
	tr := &webtransport.Transport{
		TLSClientConfig: &tls.Config{
			NextProtos:         []string{http3.NextProtoH3},
			InsecureSkipVerify: true,
			VerifyConnection: func(s tls.ConnectionState) error {
				sum := sha256.Sum256(s.PeerCertificates[0].Raw)
				if base64.StdEncoding.EncodeToString(sum[:]) != pinned {
					return errors.New("certificate hash mismatch")
				}
				return nil
			},
		},
		QUICConfig: &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	t.Cleanup(func() { tr.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, sess, err := tr.Dial(ctx, "https://"+udp.LocalAddr().String()+"/owd?interval=5&size=100", nil)
	if err != nil {
		t.Fatal(err)
	}

	ping := []byte{typePing}
	ping = binary.BigEndian.AppendUint32(ping, 1)
	ping = binary.BigEndian.AppendUint64(ping, math.Float64bits(float64(time.Now().UnixMicro())/1000))
	if err := sess.SendDatagram(ping); err != nil {
		t.Fatal(err)
	}

	var beacons int
	var lastSeq uint32
	var gotPong bool
	for beacons < 10 || !gotPong {
		d, err := sess.ReceiveDatagram(ctx)
		if err != nil {
			t.Fatalf("after %d beacons, pong %v: %v", beacons, gotPong, err)
		}
		switch d[0] {
		case typeBeacon:
			if len(d) != 100 {
				t.Errorf("beacon is %d bytes, want 100", len(d))
			}
			seq := binary.BigEndian.Uint32(d[1:])
			if beacons > 0 && seq <= lastSeq {
				t.Errorf("seq %d after %d", seq, lastSeq)
			}
			lastSeq = seq
			owd := time.Since(time.UnixMicro(int64(binary.BigEndian.Uint64(d[5:]))))
			if owd < 0 || owd > time.Second {
				t.Errorf("loopback one-way delay %v", owd)
			}
			beacons++
		case typePong:
			if len(d) != pongLen || binary.BigEndian.Uint32(d[1:]) != 1 {
				t.Errorf("bad pong %v", d)
			}
			gotPong = true
		}
	}
}
