// Package proxyproto reads the PROXY protocol v2 header a layer-4 load
// balancer puts before a connection's bytes (the global plane's Network
// Load Balancer with proxy_protocol_v2, ADR-0035), so that the router sees
// each client's address rather than the balancer's: in its logs and in
// the X-Forwarded-For it gives the cells.
//
// The header is required, and believed, only on connections from the
// balancer's own addresses (Listener.From, its subnets); a connection
// from anywhere else is served as it is, so a client cannot name another
// address by sending a header itself (its bytes reach TLS, which refuses
// them). The header is read on the connection's first use, in the
// goroutine serving it, never in Accept, so a slow or silent peer holds
// up nobody else.
package proxyproto

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// ParseFrom parses a setting listing the load balancer's subnets, a
// comma-separated list of CIDRs (Listener.From); setting names it in the
// error.
func ParseFrom(setting, raw string) ([]netip.Prefix, error) {
	var from []netip.Prefix
	for part := range strings.SplitSeq(raw, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("%s must be a comma-separated list of CIDRs (the load balancer's subnets), not %q", setting, part)
		}
		from = append(from, p.Masked())
	}
	return from, nil
}

// Listen opens a TCP listener on addr whose connections from the load
// balancer's subnets (from) begin with its PROXY v2 header; with no
// subnets it is the plain listener. A server's TLS goes on top, since the
// header comes first.
func Listen(ctx context.Context, addr string, from []netip.Prefix) (net.Listener, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("proxyproto: %w", err)
	}
	if len(from) == 0 {
		return ln, nil
	}
	return &Listener{Listener: ln, From: from}, nil
}

// DefaultTimeout bounds the reading of a header.
const DefaultTimeout = 10 * time.Second

// maxPayload bounds the addresses and TLVs after the fixed part; a load
// balancer's are well under it (AWS adds a VPC endpoint id).
const maxPayload = 2048

// signature opens every v2 header.
var signature = []byte("\r\n\r\n\x00\r\nQUIT\n")

// Listener accepts connections whose peers in From begin with a PROXY v2
// header.
type Listener struct {
	net.Listener
	From    []netip.Prefix
	Timeout time.Duration // reading a header; 0: DefaultTimeout
}

// Accept implements net.Listener. Its error is the inner listener's, as
// it is: http.Server tells a temporary one from a closed listener by its
// type.
func (l *Listener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err //nolint:wrapcheck // http.Server inspects the listener's error by type (net.Error) to retry or stop
	}
	if !l.trusted(c.RemoteAddr()) {
		return c, nil
	}
	timeout := l.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	return &Conn{Conn: c, timeout: timeout}, nil
}

func (l *Listener) trusted(a net.Addr) bool {
	tcp, ok := a.(*net.TCPAddr)
	if !ok {
		return false
	}
	ip, ok := netip.AddrFromSlice(tcp.IP)
	if !ok {
		return false
	}
	ip = ip.Unmap()
	for _, p := range l.From {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// Conn is a connection from the load balancer: its RemoteAddr is the
// client's, from the header, which its first Read or RemoteAddr reads.
type Conn struct {
	net.Conn
	timeout time.Duration

	once   sync.Once
	err    error
	remote net.Addr

	mu       sync.Mutex
	deadline time.Time // the read deadline the user last set, restored after the header
}

// Read implements net.Conn: the bytes after the header, or the header's
// error.
func (c *Conn) Read(b []byte) (int, error) {
	c.once.Do(c.readHeader)
	if c.err != nil {
		return 0, c.err
	}
	return c.Conn.Read(b) //nolint:wrapcheck // a net.Conn's errors are the caller's to inspect as they are (timeouts, EOF)
}

// RemoteAddr implements net.Conn: the client's address the header names,
// or the balancer's when it names none (a health check's LOCAL) or could
// not be read.
func (c *Conn) RemoteAddr() net.Addr {
	c.once.Do(c.readHeader)
	return c.remote
}

// SetDeadline implements net.Conn.
func (c *Conn) SetDeadline(t time.Time) error {
	c.remember(t)
	return c.Conn.SetDeadline(t) //nolint:wrapcheck // passed through as net.Conn's
}

// SetReadDeadline implements net.Conn.
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.remember(t)
	return c.Conn.SetReadDeadline(t) //nolint:wrapcheck // passed through as net.Conn's
}

func (c *Conn) remember(t time.Time) {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
}

// readHeader reads the header within the timeout, then gives the read
// deadline back to what the user set.
func (c *Conn) readHeader() {
	c.remote = c.Conn.RemoteAddr()
	if err := c.Conn.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
		c.err = fmt.Errorf("proxyproto: header deadline: %w", err)
		return
	}
	remote, err := read(c.Conn)
	c.mu.Lock()
	deadline := c.deadline
	c.mu.Unlock()
	if err == nil {
		err = c.Conn.SetReadDeadline(deadline)
	}
	if err != nil {
		c.err = fmt.Errorf("proxyproto: from %s: %w", c.remote, err)
		return
	}
	if remote != nil {
		c.remote = remote
	}
}

// read reads one v2 header and returns the source address it names, nil
// for none: a LOCAL command (the balancer's own health check), or an
// address family other than TCP over IPv4 or IPv6.
func read(r io.Reader) (net.Addr, error) {
	var fixed [16]byte
	if _, err := io.ReadFull(r, fixed[:]); err != nil {
		return nil, fmt.Errorf("header: %w", err)
	}
	if !bytes.Equal(fixed[:12], signature) {
		return nil, errors.New("no PROXY v2 header from the load balancer")
	}
	if fixed[12]>>4 != 2 {
		return nil, fmt.Errorf("version %d, want 2", fixed[12]>>4)
	}
	command := fixed[12] & 0xf
	if command > 1 {
		return nil, fmt.Errorf("command %d, want LOCAL or PROXY", command)
	}
	n := binary.BigEndian.Uint16(fixed[14:16])
	if n > maxPayload {
		return nil, fmt.Errorf("%d bytes of addresses, more than %d", n, maxPayload)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("addresses: %w", err)
	}
	if command == 0 {
		return nil, nil
	}
	return source(fixed[13], payload)
}

// source is the source address of a PROXY command's payload.
func source(family byte, payload []byte) (net.Addr, error) {
	var size int
	switch family {
	case 0x11: // TCP over IPv4: source, destination, their ports
		size = 4
	case 0x21: // TCP over IPv6
		size = 16
	default:
		return nil, nil
	}
	if len(payload) < 2*size+4 {
		return nil, fmt.Errorf("%d bytes of addresses, too few for family %#x", len(payload), family)
	}
	ip := net.IP(bytes.Clone(payload[:size]))
	port := binary.BigEndian.Uint16(payload[2*size : 2*size+2])
	return &net.TCPAddr{IP: ip, Port: int(port)}, nil
}
