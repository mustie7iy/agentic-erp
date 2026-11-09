package egress

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The webhook client's bounds (ADR-0041).
const (
	ConnectTimeout = 5 * time.Second  // to dial and to finish the TLS handshake
	Timeout        = 10 * time.Second // the whole exchange, response body included
	MaxResponse    = 64 << 10         // bytes of a response body read, and of its headers
)

// Client calls endpoints tenants register: https only, to public unicast
// addresses only, directly, without following redirects (a redirect would
// lead the request to an address never checked as a URL), trusting the
// system roots (never ERP_TLS_CA, which names the cell's own services)
// and presenting no certificate. A client made with an option trusts the
// option's roots in their place, for every address. Each client bounds
// what it reads of an answer: the webhook client MaxResponse, a client
// made with ResponseLimit its own (ADR-0061 §3: an e-invoicing provider's
// documents, up to 10 MiB).
type Client struct {
	http  *http.Client
	limit int
}

// Option changes how New makes a Client.
type Option func(*options)

type options struct {
	loopback bool
	receiver *netip.AddrPort // WalkthroughReceiver's address; nil: none
	roots    *x509.CertPool
	limit    *int           // ResponseLimit's; nil: MaxResponse
	timeout  *time.Duration // CallTimeout's; nil: Timeout
}

// ResponseLimit bounds the bytes of an answer's body the client reads to
// n instead of MaxResponse (headers stay bounded by MaxResponse): a
// client that fetches documents rather than acknowledgements, an
// e-invoicing provider's (ADR-0061 §3, MaxDocument). New refuses n below
// 1.
func ResponseLimit(n int) Option {
	return func(o *options) { o.limit = &n }
}

// CallTimeout bounds the whole exchange, the answer's body included, to d
// instead of Timeout: a client whose answers are documents of megabytes
// (ADR-0061 §3: 60 s per call). New refuses d below a second.
func CallTimeout(d time.Duration) Option {
	return func(o *options) { o.timeout = &d }
}

// LoopbackForTests lets the client reach loopback addresses, and only
// them besides public ones, trusting roots instead of the system's: a test
// server (httptest.NewTLSServer) is on loopback with its own certificate.
// No configuration sets it, and New refuses it outside a test binary.
func LoopbackForTests(roots *x509.CertPool) Option {
	return func(o *options) { o.loopback, o.roots = true, roots }
}

// WalkthroughReceiver lets the client reach one loopback address and
// port besides public addresses, trusting roots (the receiver's own
// certificate) instead of the system's: a stand-in the pilot walkthrough
// starts on its machine (scripts/walkthrough.sh), the webhook receiver,
// the one local delivery the product owner allowed (2026-11-04, ADR-0041
// As built), and the e-invoicing provider's stand-in (ADR-0061 As built).
// Only ERP_WALKTHROUGH_RECEIVER and ERP_WALKTHROUGH_PROVIDER set it, which
// config.Load refuses outside development. New refuses an address that is not loopback, or
// no port: the option opens one port of the machine itself, never the
// network around it. A host name is refused as the receiver's URL when it
// is written (CheckURL); one that later resolves to the receiver's address
// is dialled, and refused by TLS, the receiver's certificate naming its IP
// address alone.
func WalkthroughReceiver(addr netip.AddrPort, roots *x509.CertPool) Option {
	return func(o *options) { o.receiver, o.roots = &addr, roots }
}

// inTest tells whether the process is a test binary; tests replace it to
// prove New refuses the test option elsewhere.
var inTest = testing.Testing

// New makes a Client.
func New(opts ...Option) (*Client, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	control, err := o.control()
	if err != nil {
		return nil, err
	}
	limit, timeout, err := o.bounds()
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: ConnectTimeout, Control: control}
	return &Client{limit: limit, http: &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                  nil, // ADR-0033: a proxy would dial addresses the control never sees
			DialContext:            dialer.DialContext,
			TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: o.roots},
			TLSHandshakeTimeout:    ConnectTimeout,
			MaxResponseHeaderBytes: MaxResponse,
			ForceAttemptHTTP2:      true,
			MaxIdleConnsPerHost:    2,
			IdleConnTimeout:        90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// bounds are the client's limit on an answer's body and its time per
// call: the options' or the webhook client's.
func (o options) bounds() (int, time.Duration, error) {
	limit, timeout := MaxResponse, Timeout
	if o.limit != nil {
		limit = *o.limit
	}
	if o.timeout != nil {
		timeout = *o.timeout
	}
	switch {
	case limit < 1:
		return 0, 0, fmt.Errorf("egress: a response limit of %d bytes reads nothing", limit)
	case timeout < time.Second:
		return 0, 0, fmt.Errorf("egress: a call timeout of %v is below a second", timeout)
	}
	return limit, timeout, nil
}

// control is the dialer's check of every address dialled: RefusePrivate,
// unless an option widens it.
func (o options) control() (func(network, address string, c syscall.RawConn) error, error) {
	switch {
	case o.loopback && o.receiver != nil:
		return nil, errors.New("egress: the test option and the walkthrough's receiver exclude each other")
	case o.loopback:
		if !inTest() {
			return nil, errors.New("egress: loopback is allowed only in tests")
		}
		return allowLoopback, nil
	case o.receiver != nil:
		r := *o.receiver
		if !r.IsValid() || !r.Addr().Unmap().IsLoopback() || r.Port() == 0 {
			return nil, fmt.Errorf("egress: the walkthrough's receiver %s is not a loopback address and port", r)
		}
		if o.roots == nil {
			return nil, errors.New("egress: the walkthrough's receiver needs the roots its certificate chains to")
		}
		return allowReceiver(netip.AddrPortFrom(r.Addr().Unmap(), r.Port())), nil
	}
	return RefusePrivate, nil
}

// allowReceiver is RefusePrivate letting the one address and port of the
// walkthrough's receiver through: another port of the machine stays
// refused.
func allowReceiver(receiver netip.AddrPort) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		if ap, err := netip.ParseAddrPort(address); err == nil && netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()) == receiver {
			return nil
		}
		return RefusePrivate(network, address, c)
	}
}

// allowLoopback is RefusePrivate letting loopback addresses through, for
// tests only.
func allowLoopback(network, address string, c syscall.RawConn) error {
	if ap, err := netip.ParseAddrPort(address); err == nil && ap.Addr().Unmap().IsLoopback() {
		return nil
	}
	return RefusePrivate(network, address, c)
}

// Response is what an endpoint answered: a redirect is answered as is.
type Response struct {
	Status    int
	Header    http.Header
	Body      []byte // at most the client's limit (MaxResponse unless ResponseLimit)
	Truncated bool   // the endpoint sent more than Body holds
}

// Post sends body to rawURL with header and reads the answer, whatever its
// status. An error means no answer was had: the URL was refused, the
// address was not public, or the connection, TLS or time failed.
func (c *Client) Post(ctx context.Context, rawURL string, header http.Header, body []byte) (Response, error) {
	return c.Do(ctx, http.MethodPost, rawURL, header, body)
}

// Get reads rawURL with header, as Post does.
func (c *Client) Get(ctx context.Context, rawURL string, header http.Header) (Response, error) {
	return c.Do(ctx, http.MethodGet, rawURL, header, nil)
}

// Do sends a request of the method to rawURL with header and body (none
// when nil) and reads the answer, whatever its status, as Post says.
func (c *Client) Do(ctx context.Context, method, rawURL string, header http.Header, body []byte) (Response, error) {
	u, err := parse(rawURL)
	if err != nil {
		return Response{}, err
	}
	var content io.Reader
	if body != nil {
		content = bytes.NewReader(body) // NewRequest sets GetBody, which a retry on another connection reads
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), content)
	if err != nil {
		return Response{}, fmt.Errorf("egress: %s to %s: %w", strings.ToLower(method), u.Host, err)
	}
	if header != nil {
		req.Header = header.Clone()
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// net/http's error repeats the URL with its path and query, where
		// a token may be: only the host is kept.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return Response{}, fmt.Errorf("egress: %s to %s: %w", strings.ToLower(method), u.Host, err)
	}
	defer func() { _ = resp.Body.Close() }() // the body is read as far as wanted; closing a response body loses nothing
	read, err := io.ReadAll(io.LimitReader(resp.Body, int64(c.limit)+1))
	if err != nil {
		return Response{}, fmt.Errorf("egress: read the answer of %s: %w", u.Host, err)
	}
	r := Response{Status: resp.StatusCode, Header: resp.Header, Body: read}
	if len(read) > c.limit {
		r.Body, r.Truncated = read[:c.limit], true
	}
	return r, nil
}
