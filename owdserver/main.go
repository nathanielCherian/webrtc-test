// Command owdserver measures server→client one-way delay over WebTransport. Each
// session gets a datagram every interval carrying the server's send time. The
// page also sends ping datagrams, which the server echoes with its receive and
// send times, so the page can estimate the clock offset (NTP-style) over the same
// QUIC path. See beacon.go for the wire format.
//
// It is independent of the media server (its own Go module, no cgo), so it can run
// next to it or on any other host:
//
//	owdserver -addr :4433
//	page:      https://<host>:4433/owd?interval=20&size=200   (WebTransport, UDP)
//	           http://<host>:4433/cert-hash                    (TCP, same port number)
//
// Without -tls-cert it uses a short-lived self-signed certificate whose hash the
// page pins via serverCertificateHashes; /cert-hash serves that hash.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

// Every flag can also be set by an environment variable: --foo-bar => PUFFER_OWD_FOO_BAR.
func envName(flagName string) string {
	return "PUFFER_OWD_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

func main() {
	var (
		addr        = flag.String("addr", ":4433", "listen address: WebTransport on UDP, /cert-hash and /healthz on TCP")
		tlsCert     = flag.String("tls-cert", "", "TLS certificate file (default: rotating self-signed certificate, pinned by hash)")
		tlsKey      = flag.String("tls-key", "", "TLS key file")
		interval    = flag.Duration("interval", 20*time.Millisecond, "default beacon interval (a session can ask for 1–1000 ms with ?interval=)")
		maxSessions = flag.Int64("max-sessions", 64, "maximum concurrent sessions")
	)
	flag.VisitAll(func(f *flag.Flag) {
		if v, ok := os.LookupEnv(envName(f.Name)); ok {
			if err := f.Value.Set(v); err != nil {
				log.Fatalf("env %s: %v", envName(f.Name), err)
			}
		}
	})
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var certs *certSource
	var err error
	if *tlsCert != "" {
		certs, err = loadCert(*tlsCert, *tlsKey)
	} else {
		certs, err = newSelfSigned()
	}
	if err != nil {
		log.Fatal(err)
	}
	go certs.rotateLoop(ctx)

	udp, err := net.ListenPacket("udp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	tcp, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}

	b := newBeacon(certs, *interval, *maxSessions)
	httpSrv := &http.Server{Handler: infoHandler(certs), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = b.wt.Close()
		_ = httpSrv.Close()
	}()
	go func() {
		var err error
		if certs.selfSigned {
			// Plain HTTP: browsers would reject the self-signed certificate here.
			err = httpSrv.Serve(tcp)
		} else {
			httpSrv.TLSConfig = &tls.Config{GetCertificate: certs.getCertificate}
			err = httpSrv.ServeTLS(tcp, "", "")
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	scheme := "https"
	if certs.selfSigned {
		scheme = "http"
	}
	log.Printf("WebTransport on udp %s (path /owd), %s://%s/cert-hash on tcp", udp.LocalAddr(), scheme, tcp.Addr())
	if err := b.wt.Serve(udp); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

func newBeacon(certs *certSource, interval time.Duration, maxSessions int64) *beacon {
	h3 := &http3.Server{
		TLSConfig:  http3.ConfigureTLSConfig(&tls.Config{GetCertificate: certs.getCertificate}),
		QUICConfig: &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true, KeepAlivePeriod: 10 * time.Second},
	}
	webtransport.ConfigureHTTP3Server(h3)
	b := &beacon{
		// Any page may measure against this server; it only ever sends timestamps.
		wt:          &webtransport.Server{H3: h3, CheckOrigin: func(*http.Request) bool { return true }},
		interval:    interval,
		maxSessions: maxSessions,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/owd", b.handle)
	h3.Handler = mux
	return b
}

func infoHandler(certs *certSource) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /cert-hash", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(certs.info())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}
