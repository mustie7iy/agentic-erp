package config

import (
	"crypto/x509"
	"fmt"
	"net/netip"
	"os"
)

// Walkthrough is the pilot walkthrough's stand-ins on its own machine
// that the server may dial (scripts/walkthrough.sh): the webhook receiver,
// the one local delivery the product owner allowed (2026-11-04, ADR-0041
// As built), and the e-invoicing provider's stand-in, which the stand-in
// adapter of an einvoice_provider connector reads from (ADR-0061 §3 As
// built). Neither is a general local-delivery setting: each names one
// loopback address and port and the certificate that stand-in presents,
// and only with ERP_ENV=development; any other environment refuses to
// start with them.
type Walkthrough struct {
	Receiver netip.AddrPort // ERP_WALKTHROUGH_RECEIVER, a loopback address and port such as 127.0.0.1:18443; zero: none
	Roots    *x509.CertPool // ERP_WALKTHROUGH_RECEIVER_CA, a PEM file with the receiver's certificate, trusted for it alone
	// Provider is ERP_WALKTHROUGH_PROVIDER, the e-invoicing provider's
	// stand-in, such as 127.0.0.1:18444; zero: none.
	Provider netip.AddrPort
	// ProviderRoots is ERP_WALKTHROUGH_PROVIDER_CA, a PEM file with the
	// stand-in's certificate, trusted for it alone.
	ProviderRoots *x509.CertPool
}

// walkthrough reads the walkthrough's stand-ins once ERP_ENV is known.
func (l *loader) walkthrough(c *Config) {
	c.Walkthrough.Receiver, c.Walkthrough.Roots = l.standIn(c, "ERP_WALKTHROUGH_RECEIVER", "let webhooks reach a loopback address, for the walkthrough alone (ADR-0041)")
	c.Walkthrough.Provider, c.Walkthrough.ProviderRoots = l.standIn(c, "ERP_WALKTHROUGH_PROVIDER",
		"let the e-invoicing stand-in be read on a loopback address, for the walkthrough alone (ADR-0061)")
}

// standIn reads a walkthrough stand-in's address (the variable name) and
// certificate (name_CA): both or neither, in development only.
func (l *loader) standIn(c *Config, name, what string) (netip.AddrPort, *x509.CertPool) {
	addr, ca := l.getenv(name), l.getenv(name+"_CA")
	if addr == "" && ca == "" {
		return netip.AddrPort{}, nil
	}
	if c.Environment != Development {
		l.errs = append(l.errs, fmt.Errorf("%s and %s_CA must not be set unless ERP_ENV=development: they %s", name, name, what))
		return netip.AddrPort{}, nil
	}
	if addr == "" || ca == "" {
		l.errs = append(l.errs, fmt.Errorf("%s and %s_CA are set together: the stand-in's address and its certificate", name, name))
		return netip.AddrPort{}, nil
	}
	at, err := netip.ParseAddrPort(addr)
	if err != nil || !at.Addr().Unmap().IsLoopback() || at.Port() == 0 {
		l.errs = append(l.errs, fmt.Errorf("%s %q must be a loopback address and port, such as 127.0.0.1:18443", name, addr))
		return netip.AddrPort{}, nil
	}
	pem, err := os.ReadFile(ca) // #nosec G304 G703 -- the developer's own file, in development only
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s_CA: %w", name, err))
		return netip.AddrPort{}, nil
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		l.errs = append(l.errs, fmt.Errorf("%s_CA holds no PEM certificate", name))
		return netip.AddrPort{}, nil
	}
	return at, roots
}
