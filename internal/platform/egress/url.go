package egress

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// MaxURL is the longest endpoint URL accepted, as the subscription table's
// CHECK allows.
const MaxURL = 2048

// Resolver resolves a host name; *net.Resolver implements it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// errURL is why a URL is not one the client posts to. It never repeats the
// URL: its path or query may carry a token.
var errURL = errors.New("egress: the endpoint is not an https URL with a host, without credentials, fragment or spaces")

// parse reads an endpoint URL by the rules the client posts by.
func parse(rawURL string) (*url.URL, error) {
	if len(rawURL) > MaxURL || strings.ContainsFunc(rawURL, unicode.IsSpace) || strings.Contains(rawURL, "#") {
		return nil, errURL
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return nil, errURL
	}
	return u, nil
}

// CheckURL tells whether the client would post to rawURL, judged when a
// tenant registers it: the URL's form, and its host resolving to public
// unicast addresses only. The client checks again on every address it
// dials, since a name may later resolve elsewhere; this check lets the
// tenant know at once that an endpoint can never be reached. receiver is
// the walkthrough's receiver (WalkthroughReceiver), the zero AddrPort
// without one: a URL naming exactly its address and port is accepted, any
// other loopback URL still refused.
func CheckURL(ctx context.Context, r Resolver, rawURL string, receiver netip.AddrPort) error {
	u, err := parse(rawURL)
	if err != nil {
		return err
	}
	host := u.Hostname()
	if ip, err := netip.ParseAddr(host); err == nil {
		if receiver.IsValid() && isReceiver(u, ip, receiver) {
			return nil
		}
		if !Public(ip) {
			return fmt.Errorf("egress: host %s is not a public unicast address", ip)
		}
		return nil
	}
	addrs, err := r.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addrs) == 0 {
		return fmt.Errorf("egress: host %s does not resolve", host)
	}
	for _, a := range addrs {
		if !Public(a) {
			return fmt.Errorf("egress: host %s resolves to %s, not a public unicast address", host, a.Unmap())
		}
	}
	return nil
}

// isReceiver tells whether the URL, whose host is the address ip, names
// the receiver's address and port (443 when it names none, as https
// dials).
func isReceiver(u *url.URL, ip netip.Addr, receiver netip.AddrPort) bool {
	port := uint64(443)
	if p := u.Port(); p != "" {
		var err error
		if port, err = strconv.ParseUint(p, 10, 16); err != nil {
			return false
		}
	}
	return netip.AddrPortFrom(ip.Unmap(), uint16(port)) == netip.AddrPortFrom(receiver.Addr().Unmap(), receiver.Port())
}
