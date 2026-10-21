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

// Client posts to endpoints tenants register: https only, to public
// unicast addresses only, directly, without following redirects (a
// redirect would lead the request to an address never checked as a URL),
// trusting the system roots (never ERP_TLS_CA, which names the cell's own
// services) and presenting no certificate.
type Client struct {
	http *http.Client
}

// Option changes how New makes a Client.
type Option func(*options)

type options struct {
	loopback bool
	roots    *x509.CertPool
}

// LoopbackForTests lets the client reach loopback addresses, and only
// them besides public ones, trusting roots instead of the system's: a test
// server (httptest.NewTLSServer) is on loopback with its own certificate.
// No configuration sets it, and New refuses it outside a test binary.
func LoopbackForTests(roots *x509.CertPool) Option {
	return func(o *options) { o.loopback, o.roots = true, roots }
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
	control := RefusePrivate
	if o.loopback {
		if !inTest() {
			return nil, errors.New("egress: loopback is allowed only in tests")
		}
		control = allowLoopback
	}
	dialer := &net.Dialer{Timeout: ConnectTimeout, Control: control}
	return &Client{http: &http.Client{
		Timeout: Timeout,
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
	Body      []byte // at most MaxResponse bytes
	Truncated bool   // the endpoint sent more than Body holds
}

// Post sends body to rawURL with header and reads the answer, whatever its
// status. An error means no answer was had: the URL was refused, the
// address was not public, or the connection, TLS or time failed.
func (c *Client) Post(ctx context.Context, rawURL string, header http.Header, body []byte) (Response, error) {
	u, err := parse(rawURL)
	if err != nil {
		return Response{}, err
	}
	req := (&http.Request{
		Method:        http.MethodPost,
		URL:           u,
		Host:          u.Host,
		Header:        header.Clone(),
		Body:          io.NopCloser(bytes.NewReader(body)),
		GetBody:       func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil },
		ContentLength: int64(len(body)),
	}).WithContext(ctx)
	if req.Header == nil {
		req.Header = http.Header{}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// net/http's error repeats the URL with its path and query, where
		// a token may be: only the host is kept.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return Response{}, fmt.Errorf("egress: post to %s: %w", u.Host, err)
	}
	defer func() { _ = resp.Body.Close() }() // the body is read as far as wanted; closing a response body loses nothing
	read, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponse+1))
	if err != nil {
		return Response{}, fmt.Errorf("egress: read the answer of %s: %w", u.Host, err)
	}
	r := Response{Status: resp.StatusCode, Header: resp.Header, Body: read}
	if len(read) > MaxResponse {
		r.Body, r.Truncated = read[:MaxResponse], true
	}
	return r, nil
}
