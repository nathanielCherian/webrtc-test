package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"log"
	"math/big"
	"sync"
	"time"
)

// Browsers accept a self-signed certificate for WebTransport when the page pins
// its SHA-256 (serverCertificateHashes), but only if it is ECDSA and valid for at
// most 14 days. So a self-signed certificate is made at startup and replaced well
// before it expires; the page fetches the current hash from /cert-hash.
const (
	selfSignedValidity = 12 * 24 * time.Hour
	selfSignedRotate   = 5 * 24 * time.Hour
)

type certSource struct {
	mu         sync.RWMutex
	cert       *tls.Certificate
	hashes     []string // base64 SHA-256 of the current then previous self-signed cert
	notAfter   time.Time
	selfSigned bool
}

func loadCert(certFile, keyFile string) (*certSource, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &certSource{cert: &cert, notAfter: cert.Leaf.NotAfter}, nil
}

func newSelfSigned() (*certSource, error) {
	c := &certSource{selfSigned: true}
	return c, c.rotate()
}

func (c *certSource) rotate() error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 63))
	if err != nil {
		return err
	}
	// Backdated an hour for clients whose clocks run slow.
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "owdserver"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(selfSignedValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(der)
	hash := base64.StdEncoding.EncodeToString(sum[:])
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cert = &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	// Keep the previous hash too: a page may have fetched it just before the swap.
	c.hashes = append([]string{hash}, c.hashes[:min(len(c.hashes), 1)]...)
	c.notAfter = tmpl.NotAfter
	log.Printf("self-signed certificate sha-256 %s, valid until %s", hash, tmpl.NotAfter.Format(time.RFC3339))
	return nil
}

func (c *certSource) rotateLoop(ctx context.Context) {
	if !c.selfSigned {
		return
	}
	t := time.NewTicker(selfSignedRotate)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.rotate(); err != nil {
				log.Printf("certificate rotation failed (keeping the current one): %v", err)
			}
		}
	}
}

func (c *certSource) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cert, nil
}

// info is what /cert-hash returns. hashes is empty for a CA-issued certificate,
// which browsers verify the normal way.
func (c *certSource) info() map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return map[string]any{
		"algorithm":  "sha-256",
		"hashes":     append([]string{}, c.hashes...),
		"selfSigned": c.selfSigned,
		"notAfter":   c.notAfter,
	}
}
