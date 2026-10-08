// Command simclient is a headless receive-only viewer for testing the server
// without a browser: it negotiates like the page (POST /offer, telemetry
// DataChannel), sends real TWCC/RTCP feedback, and reports what each client
// receives alongside the server's view (rate-control target, simulcast layer,
// padding).
//
//	go run ./cmd/simclient -n 3 -duration 60s
//	go run ./cmd/simclient -rc square -center 1500 -amp 1200 -period 20 -record out.h264
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/h264writer"
)

type client struct {
	id        int
	sessionID string
	pc        *webrtc.PeerConnection

	videoBytes, videoPackets, padPackets, padBytes atomic.Int64
	frames, keyframes, audioPackets                atomic.Int64

	mu       sync.Mutex
	layer    string
	target   float64
	padBps   float64
	switches []string
	closed   string
	recorder *h264writer.H264Writer
	firstKey time.Duration
	started  time.Time
}

func main() {
	var (
		server   = flag.String("server", "http://127.0.0.1:8080", "server base URL")
		n        = flag.Int("n", 1, "number of concurrent viewers")
		duration = flag.Duration("duration", 30*time.Second, "how long to watch")
		source   = flag.String("source", "simulcast", `"simulcast" or "" (per-session encode of the default file)`)
		rc       = flag.String("rc", "gcc", "rate control: gcc, sine or square")
		center   = flag.Float64("center", 1500, "wave center (kbps)")
		amp      = flag.Float64("amp", 1000, "wave amplitude (kbps)")
		period   = flag.Float64("period", 20, "wave period (s)")
		record   = flag.String("record", "", "write client 0's received H.264 (Annex-B) to this file")
		every    = flag.Duration("report", 2*time.Second, "report interval")
	)
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *duration)
	defer cancel()

	var rcReq map[string]any
	if *rc != "gcc" {
		rcReq = map[string]any{"algorithm": *rc, "centerKbps": *center, "amplitudeKbps": *amp, "periodSec": *period}
	}
	clients := make([]*client, *n)
	for i := range clients {
		c := &client{id: i, started: time.Now()}
		if i == 0 && *record != "" {
			w, err := h264writer.New(*record)
			if err != nil {
				log.Fatal(err)
			}
			c.recorder = w
			defer w.Close()
		}
		if err := c.connect(ctx, *server, *source, rcReq); err != nil {
			log.Fatalf("client %d: %v", i, err)
		}
		defer c.pc.Close()
		clients[i] = c
	}

	t := time.NewTicker(*every)
	defer t.Stop()
	prev := make([]snapshot, len(clients))
	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			summary(clients, time.Since(start))
			return
		case <-t.C:
		}
		for i, c := range clients {
			s := c.snap()
			dt := every.Seconds()
			c.mu.Lock()
			fmt.Printf("t=%5.1fs c%d %-8s target=%6.0fk layer=%-5s pad=%5.0fk | recv video=%6.0fk fps=%4.1f keys=%d pad=%5.0fk audio=%d\n",
				time.Since(start).Seconds(), i, short(c.sessionID), c.target/1000, c.layer, c.padBps/1000,
				float64(s.videoBytes-prev[i].videoBytes)*8/dt/1000, float64(s.frames-prev[i].frames)/dt,
				s.keyframes, float64(s.padBytes-prev[i].padBytes)*8/dt/1000, s.audioPackets)
			c.mu.Unlock()
			prev[i] = s
		}
	}
}

type snapshot struct{ videoBytes, frames, keyframes, padBytes, audioPackets int64 }

func (c *client) snap() snapshot {
	return snapshot{c.videoBytes.Load(), c.frames.Load(), c.keyframes.Load(), c.padBytes.Load(), c.audioPackets.Load()}
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func summary(clients []*client, d time.Duration) {
	fmt.Printf("\n== summary after %.0fs\n", d.Seconds())
	for _, c := range clients {
		s := c.snap()
		c.mu.Lock()
		fmt.Printf("client %d (%s): %d frames (%.1f fps), %d keyframes, video %.0f kbps avg, padding %d pkts / %.0f kbps avg, audio %d pkts, first keyframe after %v, final layer %s\n",
			c.id, short(c.sessionID), s.frames, float64(s.frames)/d.Seconds(), s.keyframes, float64(s.videoBytes)*8/d.Seconds()/1000,
			c.padPackets.Load(), float64(s.padBytes)*8/d.Seconds()/1000, s.audioPackets, c.firstKey.Round(time.Millisecond), c.layer)
		for _, sw := range c.switches {
			fmt.Printf("    %s\n", sw)
		}
		if c.closed != "" {
			fmt.Printf("    server closed the session: %s\n", c.closed)
		}
		c.mu.Unlock()
	}
}

func (c *client) connect(ctx context.Context, server, source string, rc map[string]any) error {
	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		return err
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(me, ir); err != nil { // NACK, RTCP reports, TWCC feedback
		return err
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(ir))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return err
	}
	c.pc = pc
	for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if _, err := pc.AddTransceiverFromKind(kind, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			return err
		}
	}
	dc, err := pc.CreateDataChannel("telemetry", nil)
	if err != nil {
		return err
	}
	dc.OnMessage(func(m webrtc.DataChannelMessage) { c.onTelemetry(m.Data) })
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if tr.Kind() == webrtc.RTPCodecTypeVideo {
			go c.readVideo(tr)
		} else {
			go c.readAudio(tr)
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		return err
	}
	<-gathered
	body, _ := json.Marshal(map[string]any{
		"sdp": pc.LocalDescription().SDP, "type": "offer", "source": source, "rateControl": rc,
		"client": map[string]any{"userAgent": "simclient", "index": c.id},
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(server, "/")+"/offer", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("offer: %s: %s", resp.Status, raw)
	}
	var ans struct{ SDP, Type, SessionID string }
	if err := json.Unmarshal(raw, &ans); err != nil {
		return err
	}
	c.sessionID = ans.SessionID
	return pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ans.SDP})
}

func (c *client) readVideo(tr *webrtc.TrackRemote) {
	key := false
	for {
		pkt, _, err := tr.ReadRTP()
		if err != nil {
			return
		}
		if len(pkt.Payload) == 0 {
			c.padPackets.Add(1)
			c.padBytes.Add(int64(pkt.PaddingSize))
			continue
		}
		c.videoPackets.Add(1)
		c.videoBytes.Add(int64(len(pkt.Payload)))
		key = key || isKeyframeStart(pkt.Payload) // an IDR may span several slices
		if pkt.Marker {
			c.frames.Add(1)
			if key && c.keyframes.Add(1) == 1 {
				c.mu.Lock()
				c.firstKey = time.Since(c.started)
				c.mu.Unlock()
			}
			key = false
		}
		if c.recorder != nil {
			_ = c.recorder.WriteRTP(pkt)
		}
	}
}

func (c *client) readAudio(tr *webrtc.TrackRemote) {
	for {
		if _, _, err := tr.ReadRTP(); err != nil {
			return
		}
		c.audioPackets.Add(1)
	}
}

// isKeyframeStart reports whether an H.264 RTP payload starts an IDR picture.
func isKeyframeStart(p []byte) bool {
	switch t := p[0] & 0x1f; t {
	case 5:
		return true
	case 24: // STAP-A: look at each NAL
		for i := 1; i+2 < len(p); {
			size := int(p[i])<<8 | int(p[i+1])
			if i+2 < len(p) && p[i+2]&0x1f == 5 {
				return true
			}
			i += 2 + size
		}
	case 28: // FU-A start of an IDR
		return len(p) > 1 && p[1]&0x80 != 0 && p[1]&0x1f == 5
	}
	return false
}

func (c *client) onTelemetry(data []byte) {
	var m struct {
		Type        string `json:"type"`
		RateControl *struct {
			TargetBitrate float64 `json:"targetBitrate"`
		} `json:"rateControl"`
		Simulcast *struct {
			Layer      string  `json:"layer"`
			PaddingBps float64 `json:"paddingBps"`
		} `json:"simulcast"`
		Event *struct {
			Kind  string `json:"kind"`
			Value string `json:"value"`
		} `json:"event"`
	}
	if json.Unmarshal(data, &m) != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch m.Type {
	case "server-stats":
		if m.RateControl != nil {
			c.target = m.RateControl.TargetBitrate
		}
		if m.Simulcast != nil {
			c.layer, c.padBps = m.Simulcast.Layer, m.Simulcast.PaddingBps
		}
	case "server-event":
		if m.Event == nil {
			return
		}
		switch m.Event.Kind {
		case "layer", "layerPending":
			c.switches = append(c.switches, fmt.Sprintf("%5.1fs %s: %s", time.Since(c.started).Seconds(), m.Event.Kind, m.Event.Value))
		case "closed":
			c.closed = m.Event.Value
		}
	}
}
