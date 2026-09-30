// Package telemetry records per-session measurements from both the server and
// the client to a JSON-lines file, and provides encoder-side counters.
package telemetry

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Record is one line of the session log.
//
//	src:  "server" (server snapshot), "client" (payload sent by the browser),
//	      "event" (lifecycle / state change), "meta" (session setup info)
//	t:    server wall clock, unix milliseconds
type Record struct {
	T    float64 `json:"t"`
	Src  string  `json:"src"`
	Kind string  `json:"kind,omitempty"`
	Data any     `json:"data"`
}

// Logger appends Records to logs/<session>.jsonl.
type Logger struct {
	mu   sync.Mutex
	f    *os.File
	w    *bufio.Writer
	enc  *json.Encoder
	done chan struct{}
	Path string
}

// NewLogger creates the log file and starts a periodic flusher.
func NewLogger(dir, sessionID string) (*Logger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, time.Now().UTC().Format("20060102T150405Z")+"_"+sessionID+".jsonl")
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	w := bufio.NewWriterSize(f, 256*1024)
	l := &Logger{f: f, w: w, enc: json.NewEncoder(w), done: make(chan struct{}), Path: path}
	go l.flushLoop()
	return l, nil
}

// NowMs is the timestamp format used throughout the logs.
func NowMs() float64 { return float64(time.Now().UnixMicro()) / 1000.0 }

// Log writes a record. Data may be a json.RawMessage to avoid re-encoding.
func (l *Logger) Log(src, kind string, data any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	_ = l.enc.Encode(Record{T: NowMs(), Src: src, Kind: kind, Data: data})
}

func (l *Logger) flushLoop() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-l.done:
			return
		case <-t.C:
			l.mu.Lock()
			if l.f != nil {
				_ = l.w.Flush()
			}
			l.mu.Unlock()
		}
	}
}

// Close flushes and closes the file.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	close(l.done)
	_ = l.w.Flush()
	err := l.f.Close()
	l.f = nil
	return err
}
