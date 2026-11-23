// Command wmsstub is the warehouse system's stand-in of the pilot
// walkthrough (scripts/walkthrough.sh), as providerstub is the e-invoicing
// provider's: an HTTPS endpoint on a loopback port, the address
// ERP_WALKTHROUGH_WMS lets the server call (ADR-0082 As built S2q),
// speaking the stand-in adapter's protocol (wmstest.Handler). It makes a
// self-signed certificate for its address at start and writes it to the
// ca file (erp-server trusts it as ERP_WALKTHROUGH_WMS_CA; the walkthrough
// waits for the file as the sign that it listens). What it is sent is
// kept in the received directory, <id>.json the message as first sent and
// <id>.sends a line per send; what it reports back are the confirmations
// directory's files: <id>.json the confirmation, which the walkthrough
// writes, and <id>.acknowledged a line per acknowledgement, which takes it
// off the pending list. It answers requests bearing the token in the token
// file and runs until it is stopped.
//
//	wmsstub -addr 127.0.0.1:18445 -ca wms.pem -token token -received received -confirmations confirmations
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io/fs"
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

	"github.com/mustie7iy/agentic-erp/internal/integration/wmstest"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf/tlstest"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18445", "where to listen: a loopback address and port")
	ca := flag.String("ca", "", "where to write the stand-in's certificate, PEM")
	token := flag.String("token", "", "the file holding the bearer token requests must bear")
	received := flag.String("received", "", "the directory of the messages sent to the stand-in")
	confirmations := flag.String("confirmations", "", "the directory of the confirmations it reports")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, *addr, *ca, *token, dirs{received: *received, confirmations: *confirmations})
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "wmsstub:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, addr, ca, tokenFile string, store dirs) error {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil || !ap.Addr().IsLoopback() {
		return errors.New("addr: the walkthrough's warehouse system listens on a loopback address and port only")
	}
	if store.received == "" || store.confirmations == "" {
		return errors.New("-received and -confirmations name the stand-in's directories")
	}
	token, err := os.ReadFile(tokenFile) // #nosec G304 G703 -- the walkthrough names its own file
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	cert, certPEM, err := tlstest.SelfSigned(ap.Addr(), "walkthrough warehouse system")
	if err != nil {
		return err
	}
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	srv := &http.Server{Handler: wmstest.Handler(store, strings.TrimSpace(string(token))), ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	errc := make(chan error, 1)
	go func() { errc <- srv.ServeTLS(ln, "", "") }()
	if err := os.WriteFile(ca, certPEM, 0o600); err != nil { // #nosec G703 -- the walkthrough names its own file
		return fmt.Errorf("ca file: %w", err)
	}
	fmt.Println("wmsstub: listening on", addr)
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

// dirs are the stand-in's directories as its store.
type dirs struct{ received, confirmations string }

// messageID is an id the directories can hold as a file name.
var messageID = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,99}$`)

// appendLine adds a line with the time to the file.
func appendLine(path string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- an id the pattern above allows, in the walkthrough's directory
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := fmt.Fprintln(f, time.Now().UTC().Format(time.RFC3339)); err != nil {
		_ = f.Close() // the write's error is the one reported
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// Receive keeps the message as first sent and counts the send.
func (d dirs) Receive(id string, body []byte) error {
	if !messageID.MatchString(id) {
		return fmt.Errorf("received: message id %q is not one the directory holds", id)
	}
	path := filepath.Join(d.received, id+".json")
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if err := os.WriteFile(path, body, 0o600); err != nil { // #nosec G703 -- as appendLine
			return fmt.Errorf("received: %w", err)
		}
	}
	return appendLine(filepath.Join(d.received, id+".sends"))
}

// Pending lists the confirmations not yet acknowledged, by id.
func (d dirs) Pending() ([]string, error) {
	names, err := filepath.Glob(filepath.Join(d.confirmations, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("confirmations: %w", err)
	}
	sort.Strings(names)
	var out []string
	for _, name := range names {
		id := strings.TrimSuffix(filepath.Base(name), ".json")
		if !messageID.MatchString(id) {
			continue
		}
		if _, err := os.Stat(filepath.Join(d.confirmations, id+".acknowledged")); err == nil {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// Get reads a confirmation.
func (d dirs) Get(id string) ([]byte, bool, error) {
	if !messageID.MatchString(id) {
		return nil, false, nil
	}
	body, err := os.ReadFile(filepath.Join(d.confirmations, id+".json")) // #nosec G304 -- as appendLine
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("confirmations: %w", err)
	}
	return body, true, nil
}

// Acknowledge appends a line to the confirmation's acknowledgements.
func (d dirs) Acknowledge(id string) (bool, error) {
	if _, ok, err := d.Get(id); err != nil || !ok {
		return false, err
	}
	if err := appendLine(filepath.Join(d.confirmations, id+".acknowledged")); err != nil {
		return false, fmt.Errorf("confirmations: %w", err)
	}
	return true, nil
}
