// Command webhookstub is the webhook subscriber of the pilot walkthrough
// (scripts/walkthrough.sh), as sftpstub is its treasury system's SFTP
// server: an HTTPS endpoint on a loopback port, the one address
// ERP_WALKTHROUGH_RECEIVER lets the server deliver to (ADR-0041 As built,
// product owner 2026-11-04). It makes a self-signed certificate for its
// address at start and writes it to the ca file (erp-server trusts it as
// ERP_WALKTHROUGH_RECEIVER_CA; the walkthrough waits for the file as the
// sign that it listens). Each delivery is verified as a subscriber
// verifies it, with the subscription's whsec_ secret read from the secret
// file and internal/platform/webhook (Standard Webhooks, checked against
// api/webhooks-test-vectors.json), and the same signature checked against
// the body with one byte changed, which must be refused. It answers 204
// to a verified delivery and 401 to any other, and writes each one, its
// headers, body and verdicts, to the out directory as delivery-NNNN.json.
// It runs until it is stopped.
//
//	webhookstub -addr 127.0.0.1:18443 -ca receiver.pem -secret secret -out received
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/webhook"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18443", "where to listen: a loopback address and port")
	ca := flag.String("ca", "", "where to write the receiver's certificate, PEM")
	secret := flag.String("secret", "", "the file holding the subscription's whsec_ secret")
	out := flag.String("out", "", "the directory every delivery is written to")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, *addr, *ca, *secret, *out)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "webhookstub:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, addr, ca, secret, out string) error {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil || !ap.Addr().IsLoopback() {
		return errors.New("addr: the walkthrough's receiver listens on a loopback address and port only")
	}
	cert, certPEM, err := selfSigned(ap.Addr())
	if err != nil {
		return err
	}
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	r := &receiver{secret: secret, out: out}
	srv := &http.Server{Handler: r, ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	errc := make(chan error, 1)
	go func() { errc <- srv.ServeTLS(ln, "", "") }()
	if err := os.WriteFile(ca, certPEM, 0o600); err != nil { // #nosec G703 -- the walkthrough names its own file
		return fmt.Errorf("ca file: %w", err)
	}
	fmt.Println("webhookstub: listening on", addr)
	select {
	case <-ctx.Done():
		if err := srv.Close(); err != nil {
			return fmt.Errorf("close: %w", err)
		}
		return nil
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	}
}

// selfSigned is a certificate for ip, a day long, and its PEM.
func selfSigned(ip netip.Addr) (tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("key: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: "walkthrough webhook receiver"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour), IPAddresses: []net.IP{ip.AsSlice()},
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("certificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// receiver verifies and keeps the deliveries.
type receiver struct {
	secret, out string
	mu          sync.Mutex
	n           int
}

// delivery is what the walkthrough reads of one delivery.
type delivery struct {
	WebhookID     string          `json:"webhook_id"`
	DeliveryID    string          `json:"webhook_delivery_id"`
	Timestamp     string          `json:"webhook_timestamp"`
	Signature     string          `json:"webhook_signature"`
	ContentType   string          `json:"content_type"`
	Event         json.RawMessage `json:"event"`
	Verified      bool            `json:"verified"`
	VerifyError   string          `json:"verify_error,omitempty"`
	TamperRefused bool            `json:"tampered_body_refused"`
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "webhookstub: POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if err != nil {
		http.Error(w, "webhookstub: "+err.Error(), http.StatusBadRequest)
		return
	}
	d := delivery{WebhookID: req.Header.Get(webhook.HeaderID), DeliveryID: req.Header.Get("webhook-delivery-id"), Timestamp: req.Header.Get(webhook.HeaderTimestamp),
		Signature: req.Header.Get(webhook.HeaderSignature), ContentType: req.Header.Get("Content-Type"), Event: body}
	if !json.Valid(body) {
		d.Event = nil
	}
	if v, err := r.verifier(); err != nil {
		d.VerifyError = err.Error()
	} else if err := v.Verify(req.Header, body); err != nil {
		d.VerifyError = err.Error()
	} else {
		d.Verified = true
		d.TamperRefused = errors.Is(v.Verify(req.Header, tampered(body)), webhook.ErrSignature)
	}
	if err := r.keep(d); err != nil {
		fmt.Fprintln(os.Stderr, "webhookstub:", err)
		http.Error(w, "webhookstub: "+err.Error(), http.StatusInternalServerError)
		return
	}
	fmt.Printf("webhookstub: delivery %s verified %v\n", d.WebhookID, d.Verified)
	if !d.Verified {
		http.Error(w, "webhookstub: not verified: "+d.VerifyError, http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// verifier holds the subscription's secret as the walkthrough wrote it.
func (r *receiver) verifier() (webhook.Verifier, error) {
	raw, err := os.ReadFile(r.secret) // #nosec G304 -- the walkthrough names its own file
	if err != nil {
		return webhook.Verifier{}, fmt.Errorf("secret: %w", err)
	}
	s, err := webhook.ParseSecret(strings.TrimSpace(string(raw)))
	if err != nil {
		return webhook.Verifier{}, fmt.Errorf("secret: %w", err)
	}
	return webhook.Verifier{Secrets: []webhook.Secret{s}}, nil
}

// tampered is body with its first byte changed, so it always differs from
// body (a byte is added to an empty one).
func tampered(body []byte) []byte {
	if len(body) == 0 {
		return []byte{0}
	}
	t := append([]byte(nil), body...)
	t[0] ^= 1
	return t
}

// keep writes the delivery to the out directory, numbered in arrival
// order.
func (r *receiver) keep(d delivery) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("delivery: %w", err)
	}
	name := filepath.Join(r.out, fmt.Sprintf("delivery-%04d.json", r.n))
	if err := os.WriteFile(name, b, 0o600); err != nil { // #nosec G304 G703 -- the walkthrough's own directory
		return fmt.Errorf("keep %s: %w", name, err)
	}
	return nil
}
