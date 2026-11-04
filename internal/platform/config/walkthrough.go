package config

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net/netip"
	"os"
)

// Walkthrough is the one local webhook delivery the product owner allowed
// (2026-11-04, ADR-0041 As built): the pilot walkthrough
// (scripts/walkthrough.sh) starts a webhook receiver on its own machine
// and names it here, so a subscription may point at it and the dispatcher
// may dial it. It is not a general local-delivery setting: it names one
// loopback address and port and the certificate that receiver presents,
// and only with ERP_ENV=development; any other environment refuses to
// start with it.
type Walkthrough struct {
	Receiver netip.AddrPort // ERP_WALKTHROUGH_RECEIVER, a loopback address and port such as 127.0.0.1:18443; zero: none
	Roots    *x509.CertPool // ERP_WALKTHROUGH_RECEIVER_CA, a PEM file with the receiver's certificate, trusted for it alone
}

// walkthrough reads the walkthrough's receiver once ERP_ENV is known.
func (l *loader) walkthrough(c *Config) {
	addr, ca := l.getenv("ERP_WALKTHROUGH_RECEIVER"), l.getenv("ERP_WALKTHROUGH_RECEIVER_CA")
	if addr == "" && ca == "" {
		return
	}
	if c.Environment != Development {
		l.errs = append(l.errs, errors.New("ERP_WALKTHROUGH_RECEIVER and ERP_WALKTHROUGH_RECEIVER_CA must not be set unless ERP_ENV=development: they let webhooks reach a loopback address, for the walkthrough alone (ADR-0041)"))
		return
	}
	if addr == "" || ca == "" {
		l.errs = append(l.errs, errors.New("ERP_WALKTHROUGH_RECEIVER and ERP_WALKTHROUGH_RECEIVER_CA are set together: the receiver's address and its certificate"))
		return
	}
	receiver, err := netip.ParseAddrPort(addr)
	if err != nil || !receiver.Addr().Unmap().IsLoopback() || receiver.Port() == 0 {
		l.errs = append(l.errs, fmt.Errorf("ERP_WALKTHROUGH_RECEIVER %q must be a loopback address and port, such as 127.0.0.1:18443", addr))
		return
	}
	pem, err := os.ReadFile(ca) // #nosec G304 G703 -- the developer's own file, in development only
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("ERP_WALKTHROUGH_RECEIVER_CA: %w", err))
		return
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		l.errs = append(l.errs, errors.New("ERP_WALKTHROUGH_RECEIVER_CA holds no PEM certificate"))
		return
	}
	c.Walkthrough = Walkthrough{Receiver: receiver, Roots: roots}
}
