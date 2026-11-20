// Command providerstub is the e-invoicing provider's stand-in of the pilot
// walkthrough (scripts/walkthrough.sh), as webhookstub is its webhook
// subscriber: an HTTPS endpoint on a loopback port, the address
// ERP_WALKTHROUGH_PROVIDER lets the server read from (ADR-0061 As built),
// speaking the stand-in adapter's protocol (einvoicetest.Handler). It
// makes a self-signed certificate for its address at start and writes it
// to the ca file (erp-server trusts it as ERP_WALKTHROUGH_PROVIDER_CA; the
// walkthrough waits for the file as the sign that it listens). Its
// messages are the inbox directory's files: <id>.xml the document,
// <id>.receiver the participant it is addressed to, and <id>.acknowledged
// a line per acknowledgement, which takes it off the pending list. It
// answers requests bearing the token in the token file and runs until it
// is stopped. With -evatra it also emulates Evatra's API under /evatra
// (einvoicetest.Evatra, ADR-0061 As built E1) for the account the file
// holds as JSON ({"apiKey", "email", "companyId"}), over the documents of
// the -evatra-inbox directory, in the inbox's form (Evatra keeps no
// acknowledgement, so it lists them all): the walkthrough's Evatra
// connector reads from there, as the server replaces Evatra's hosts with
// the stand-in in the walkthrough alone.
//
//	providerstub -addr 127.0.0.1:18444 -ca provider.pem -token token -inbox inbox [-evatra evatra.json -evatra-inbox evatra]
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
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/einvoicing/einvoicetest"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18444", "where to listen: a loopback address and port")
	ca := flag.String("ca", "", "where to write the stand-in's certificate, PEM")
	token := flag.String("token", "", "the file holding the bearer token requests must bear")
	inbox := flag.String("inbox", "", "the directory of the messages")
	evatra := flag.String("evatra", "", "the file holding the Evatra account to emulate, JSON; empty: none")
	evatraInbox := flag.String("evatra-inbox", "", "the directory of the documents Evatra received")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, *addr, *ca, *token, *inbox, evatraFiles{account: *evatra, inbox: *evatraInbox})
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "providerstub:", err)
		os.Exit(1)
	}
}

// evatraFiles are Evatra's emulation's: its account file and its inbox
// directory; no account file: none.
type evatraFiles struct{ account, inbox string }

func run(ctx context.Context, addr, ca, tokenFile, inbox string, evatra evatraFiles) error {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil || !ap.Addr().IsLoopback() {
		return errors.New("addr: the walkthrough's provider listens on a loopback address and port only")
	}
	token, err := os.ReadFile(tokenFile) // #nosec G304 G703 -- the walkthrough names its own file
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	handler, err := handler(dir(inbox), strings.TrimSpace(string(token)), evatra)
	if err != nil {
		return err
	}
	cert, certPEM, err := selfSigned(ap.Addr())
	if err != nil {
		return err
	}
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	errc := make(chan error, 1)
	go func() { errc <- srv.ServeTLS(ln, "", "") }()
	if err := os.WriteFile(ca, certPEM, 0o600); err != nil { // #nosec G703 -- the walkthrough names its own file
		return fmt.Errorf("ca file: %w", err)
	}
	fmt.Println("providerstub: listening on", addr)
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

// handler serves the stand-in's protocol at the root and, with an Evatra
// account file, Evatra's API under /evatra.
func handler(store dir, token string, evatra evatraFiles) (http.Handler, error) {
	standIn := einvoicetest.Handler(store, token)
	if evatra.account == "" {
		return standIn, nil
	}
	if evatra.inbox == "" {
		return nil, errors.New("evatra: -evatra-inbox names the directory of its documents")
	}
	raw, err := os.ReadFile(evatra.account) // #nosec G304 G703 -- the walkthrough names its own file
	if err != nil {
		return nil, fmt.Errorf("evatra: %w", err)
	}
	var account struct {
		APIKey    string `json:"apiKey"`
		Email     string `json:"email"`
		CompanyID string `json:"companyId"`
	}
	if err := json.Unmarshal(raw, &account); err != nil || account.APIKey == "" || account.Email == "" || account.CompanyID == "" {
		return nil, errors.New("evatra: the file holds no account: a JSON object with apiKey, email and companyId")
	}
	mux := http.NewServeMux()
	mux.Handle("/", standIn)
	mux.Handle("/evatra/", http.StripPrefix("/evatra", (&einvoicetest.Evatra{Store: dir(evatra.inbox),
		Account: einvoicetest.EvatraAccount{APIKey: account.APIKey, Email: account.Email, CompanyID: account.CompanyID}}).Handler()))
	return mux, nil
}

// selfSigned is a certificate for ip, a day long, and its PEM.
func selfSigned(ip netip.Addr) (tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("key: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: "walkthrough e-invoicing provider"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour), IPAddresses: []net.IP{ip.AsSlice()},
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("certificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// dir is the inbox directory as the stand-in's store.
type dir string

// messageID is an id the directory can hold as a file name.
var messageID = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,99}$`)

func (d dir) path(id, ext string) string { return filepath.Join(string(d), id+ext) }

// Pending lists the documents not yet acknowledged, by id.
func (d dir) Pending() ([]einvoicetest.Message, error) {
	names, err := filepath.Glob(filepath.Join(string(d), "*.xml"))
	if err != nil {
		return nil, fmt.Errorf("inbox: %w", err)
	}
	sort.Strings(names)
	var out []einvoicetest.Message
	for _, name := range names {
		id := strings.TrimSuffix(filepath.Base(name), ".xml")
		if !messageID.MatchString(id) {
			continue
		}
		if _, err := os.Stat(d.path(id, ".acknowledged")); err == nil {
			continue
		}
		receiver, err := os.ReadFile(d.path(id, ".receiver")) // #nosec G304 -- an id the pattern above allows, in the walkthrough's directory
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("inbox: %w", err)
		}
		out = append(out, einvoicetest.Message{ID: id, Receiver: strings.TrimSpace(string(receiver))})
	}
	return out, nil
}

// Get reads a document.
func (d dir) Get(id string) ([]byte, bool, error) {
	if !messageID.MatchString(id) {
		return nil, false, nil
	}
	body, err := os.ReadFile(d.path(id, ".xml")) // #nosec G304 -- as Pending
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inbox: %w", err)
	}
	return body, true, nil
}

// Acknowledge appends a line to the document's acknowledgements.
func (d dir) Acknowledge(id string) (bool, error) {
	if _, ok, err := d.Get(id); err != nil || !ok {
		return false, err
	}
	f, err := os.OpenFile(d.path(id, ".acknowledged"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- as Pending
	if err != nil {
		return false, fmt.Errorf("inbox: %w", err)
	}
	if _, err := fmt.Fprintln(f, time.Now().UTC().Format(time.RFC3339)); err != nil {
		_ = f.Close() // the write's error is the one reported
		return false, fmt.Errorf("inbox: %w", err)
	}
	if err := f.Close(); err != nil {
		return false, fmt.Errorf("inbox: %w", err)
	}
	return true, nil
}
