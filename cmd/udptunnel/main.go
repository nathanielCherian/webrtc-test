// Command udptunnel carries UDP datagrams over a TCP connection (for example an
// `ssh -L` port forward), keeping datagram boundaries, so a browser on a machine
// that can only reach the server over SSH can still use WebRTC over UDP.
//
// Each end still speaks UDP; only the hop between them is TCP, so loss and delay
// on that hop behave like TCP (retransmitted, in order), not like a real UDP path.
//
//	on the server:       udptunnel remote -tcp 127.0.0.1:50001 -udp 127.0.0.1:50000
//	on your machine:     ssh -L 8080:localhost:8080 -L 50001:localhost:50001 server
//	                     udptunnel local -udp 127.0.0.1:50000 -tcp 127.0.0.1:50001
//	server flags:        --public-ip 127.0.0.1   (ICE candidates point at your local relay)
//
// Every UDP peer on the local side (one per browser PeerConnection) gets its own
// TCP connection and, on the remote side, its own UDP socket, so the server sees
// one distinct address per peer.
package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxDatagram = 65535
	idleTimeout = 2 * time.Minute
)

func main() {
	if len(os.Args) < 2 || (os.Args[1] != "local" && os.Args[1] != "remote") {
		fmt.Fprintln(os.Stderr, "usage: udptunnel local|remote -udp host:port -tcp host:port")
		os.Exit(2)
	}
	mode := os.Args[1]
	fs := flag.NewFlagSet(mode, flag.ExitOnError)
	udpAddr := fs.String("udp", "127.0.0.1:50000", "local: UDP address to listen on; remote: UDP address to forward to (the server's media port)")
	tcpAddr := fs.String("tcp", "127.0.0.1:50001", "local: TCP address to connect to (the ssh -L port); remote: TCP address to listen on")
	verbose := fs.Bool("v", false, "log every peer")
	_ = fs.Parse(os.Args[2:])
	var err error
	if mode == "local" {
		err = runLocal(*udpAddr, *tcpAddr, *verbose)
	} else {
		err = runRemote(*tcpAddr, *udpAddr, *verbose)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// writeFrame sends one datagram as [2-byte big-endian length][payload].
func writeFrame(w io.Writer, b []byte) error {
	if len(b) > maxDatagram {
		return errors.New("datagram too large")
	}
	buf := make([]byte, 2+len(b))
	binary.BigEndian.PutUint16(buf, uint16(len(b)))
	copy(buf[2:], b)
	_, err := w.Write(buf)
	return err
}

func readFrame(r *bufio.Reader, buf []byte) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if _, err := io.ReadFull(r, buf[:n]); err != nil {
		return nil, err
	}
	return buf[:n], nil
}

type counters struct{ up, down, upBytes, downBytes atomic.Int64 }

func (c *counters) report(prefix string) {
	for range time.Tick(10 * time.Second) {
		log.Printf("%s: to server %d pkts / %.1f MB, from server %d pkts / %.1f MB", prefix,
			c.up.Load(), float64(c.upBytes.Load())/1e6, c.down.Load(), float64(c.downBytes.Load())/1e6)
	}
}

// runLocal listens for UDP from the browser and tunnels each peer over its own TCP connection.
func runLocal(udpAddr, tcpAddr string, verbose bool) error {
	laddr, err := net.ResolveUDPAddr("udp", udpAddr)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", laddr)
	if err != nil {
		return err
	}
	_ = conn.SetReadBuffer(4 << 20)
	_ = conn.SetWriteBuffer(4 << 20)
	log.Printf("local: UDP %s <-> TCP %s", udpAddr, tcpAddr)
	var c counters
	go c.report("local")

	type peer struct {
		tcp  net.Conn
		mu   sync.Mutex
		last atomic.Int64
	}
	var mu sync.Mutex
	peers := map[string]*peer{}
	buf := make([]byte, maxDatagram)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			return err
		}
		key := from.String()
		mu.Lock()
		p := peers[key]
		if p == nil {
			t, err := net.Dial("tcp", tcpAddr)
			if err != nil {
				mu.Unlock()
				log.Printf("local: connect %s: %v (is the ssh -L forward up?)", tcpAddr, err)
				continue
			}
			if tc, ok := t.(*net.TCPConn); ok {
				_ = tc.SetNoDelay(true)
			}
			p = &peer{tcp: t}
			peers[key] = p
			if verbose {
				log.Printf("local: new peer %s", key)
			}
			go func() { // server -> browser
				r := bufio.NewReaderSize(t, 1<<16)
				b := make([]byte, maxDatagram)
				for {
					d, err := readFrame(r, b)
					if err != nil {
						break
					}
					p.last.Store(time.Now().UnixNano())
					c.down.Add(1)
					c.downBytes.Add(int64(len(d)))
					_, _ = conn.WriteToUDP(d, from)
				}
				t.Close()
				mu.Lock()
				delete(peers, key)
				mu.Unlock()
				if verbose {
					log.Printf("local: peer %s closed", key)
				}
			}()
			go func() { // close idle peers
				for range time.Tick(idleTimeout / 4) {
					if time.Since(time.Unix(0, p.last.Load())) > idleTimeout {
						t.Close()
						return
					}
				}
			}()
		}
		mu.Unlock()
		p.last.Store(time.Now().UnixNano())
		c.up.Add(1)
		c.upBytes.Add(int64(n))
		p.mu.Lock()
		err = writeFrame(p.tcp, buf[:n])
		p.mu.Unlock()
		if err != nil {
			p.tcp.Close()
		}
	}
}

// runRemote accepts tunnel connections and gives each its own UDP socket to the server.
func runRemote(tcpAddr, udpAddr string, verbose bool) error {
	raddr, err := net.ResolveUDPAddr("udp", udpAddr)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", tcpAddr)
	if err != nil {
		return err
	}
	log.Printf("remote: TCP %s <-> UDP %s", tcpAddr, udpAddr)
	var c counters
	go c.report("remote")
	for {
		t, err := ln.Accept()
		if err != nil {
			return err
		}
		if tc, ok := t.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
		}
		u, err := net.DialUDP("udp", nil, raddr)
		if err != nil {
			log.Printf("remote: %v", err)
			t.Close()
			continue
		}
		_ = u.SetReadBuffer(4 << 20)
		if verbose {
			log.Printf("remote: new peer %s via UDP %s", t.RemoteAddr(), u.LocalAddr())
		}
		go func() { // browser -> server
			r := bufio.NewReaderSize(t, 1<<16)
			b := make([]byte, maxDatagram)
			for {
				d, err := readFrame(r, b)
				if err != nil {
					break
				}
				c.up.Add(1)
				c.upBytes.Add(int64(len(d)))
				_, _ = u.Write(d)
			}
			u.Close()
			t.Close()
		}()
		go func() { // server -> browser
			b := make([]byte, maxDatagram)
			for {
				_ = u.SetReadDeadline(time.Now().Add(idleTimeout))
				n, err := u.Read(b)
				if err != nil {
					break
				}
				c.down.Add(1)
				c.downBytes.Add(int64(n))
				if writeFrame(t, b[:n]) != nil {
					break
				}
			}
			u.Close()
			t.Close()
			if verbose {
				log.Printf("remote: peer %s closed", t.RemoteAddr())
			}
		}()
	}
}
