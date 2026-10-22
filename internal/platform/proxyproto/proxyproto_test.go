package proxyproto

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"
)

// header is a v2 header: version and command, family, then payload.
func header(verCmd, family byte, payload []byte) []byte {
	h := append(bytes.Clone(signature), verCmd, family, 0, 0)
	binary.BigEndian.PutUint16(h[14:], uint16(len(payload)))
	return append(h, payload...)
}

func v4(src string, srcPort uint16) []byte {
	p := append(netip.MustParseAddr(src).AsSlice(), netip.MustParseAddr("10.0.0.1").AsSlice()...)
	return binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(p, srcPort), 443)
}

func v6(src string, srcPort uint16) []byte {
	p := append(netip.MustParseAddr(src).AsSlice(), netip.MustParseAddr("fd00::1").AsSlice()...)
	return binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(p, srcPort), 443)
}

// serve accepts one connection on a loopback listener trusting from,
// sends it what the client writes, and returns the accepted connection.
func serve(t *testing.T, from []netip.Prefix, timeout time.Duration, send []byte) net.Conn {
	t.Helper()
	inner, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() }) // the test is over
	l := &Listener{Listener: inner, From: from, Timeout: timeout}
	client, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", inner.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() }) // the test is over
	if _, err := client.Write(send); err != nil {
		t.Fatal(err)
	}
	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() }) // the test is over
	return c
}

var loopback = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}

// TestHeader: from the load balancer, the header's source is the remote
// address and the bytes after it are the connection's.
func TestHeader(t *testing.T) {
	for name, c := range map[string]struct {
		send   []byte
		remote string // "" for the balancer's own
	}{
		"IPv4":            {header(0x21, 0x11, v4("203.0.113.7", 51000)), "203.0.113.7:51000"},
		"IPv6":            {header(0x21, 0x21, v6("2001:db8::7", 51000)), "[2001:db8::7]:51000"},
		"with TLVs":       {header(0x21, 0x11, append(v4("203.0.113.7", 51000), 0xea, 0, 1, 0)), "203.0.113.7:51000"},
		"health check":    {header(0x20, 0x00, nil), ""},
		"unknown family":  {header(0x21, 0x31, []byte("/tmp/a")), ""},
		"local with data": {header(0x20, 0x11, v4("203.0.113.7", 51000)), ""},
	} {
		t.Run(name, func(t *testing.T) {
			conn := serve(t, loopback, 0, append(c.send, "hello"...))
			got := make([]byte, 5)
			if _, err := io.ReadFull(conn, got); err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != "hello" {
				t.Errorf("bytes after the header: got %q, want hello", got)
			}
			want := c.remote
			if want == "" {
				want = conn.(*Conn).Conn.RemoteAddr().String()
			}
			if r := conn.RemoteAddr().String(); r != want {
				t.Errorf("remote address: got %s, want %s", r, want)
			}
		})
	}
}

// TestRefused: from the load balancer, a connection without a valid header
// fails on its first read and keeps the balancer's address.
func TestRefused(t *testing.T) {
	for name, c := range map[string]struct {
		send []byte
		want string
	}{
		"no header":     {[]byte("\x16\x03\x01 a TLS hello, not a header"), "no PROXY v2 header"},
		"version 1":     {header(0x11, 0x11, v4("203.0.113.7", 1)), "version 1"},
		"command":       {header(0x22, 0x11, v4("203.0.113.7", 1)), "command 2"},
		"too long":      {header(0x21, 0x11, make([]byte, maxPayload+1)), "more than"},
		"short IPv4":    {header(0x21, 0x11, make([]byte, 11)), "too few"},
		"short IPv6":    {header(0x21, 0x21, make([]byte, 35)), "too few"},
		"cut short":     {header(0x21, 0x11, v4("203.0.113.7", 1))[:20], "addresses"},
		"cut in header": {signature[:8], "header"},
	} {
		t.Run(name, func(t *testing.T) {
			conn := serve(t, loopback, 200*time.Millisecond, c.send)
			_, err := conn.Read(make([]byte, 1))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("read: got %v, want %q", err, c.want)
			}
			if r, own := conn.RemoteAddr().String(), conn.(*Conn).Conn.RemoteAddr().String(); r != own {
				t.Errorf("remote address: got %s, want the balancer's %s", r, own)
			}
		})
	}
}

// TestUntrusted: from anyone else, the bytes are the connection's, a
// header among them, and the address is the peer's own.
func TestUntrusted(t *testing.T) {
	send := header(0x21, 0x11, v4("203.0.113.7", 51000))
	conn := serve(t, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, 0, send)
	if _, ok := conn.(*Conn); ok {
		t.Fatal("a connection from outside From was read for a header")
	}
	got := make([]byte, len(send))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, send) {
		t.Errorf("bytes: got %q, want the client's own", got)
	}
}

// TestDeadline: the header's own deadline gives way to the user's, set
// before or after it.
func TestDeadline(t *testing.T) {
	t.Run("set before", func(t *testing.T) {
		conn := serve(t, loopback, time.Minute, header(0x20, 0, nil))
		if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		_, err := conn.Read(make([]byte, 1))
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Errorf("read: got %v, want the user's deadline", err)
		}
	})
	t.Run("none set", func(t *testing.T) {
		// The header's deadline is far shorter than the read below waits, so
		// a deadline left in place ends it long before the timer: wide
		// margins, for loaded runners.
		conn := serve(t, loopback, 10*time.Millisecond, header(0x20, 0, nil))
		conn.RemoteAddr()                                                                 // reads the header
		time.AfterFunc(500*time.Millisecond, func() { _ = conn.SetDeadline(time.Now()) }) // ends the read below
		start := time.Now()
		_, err := conn.Read(make([]byte, 1))
		if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
			t.Errorf("read: ended after %v with %v, want the header's deadline lifted", elapsed, err)
		}
	})
}

// fakeListener hands out one connection.
type fakeListener struct {
	net.Listener
	conn net.Conn
	err  error
}

func (f *fakeListener) Accept() (net.Conn, error) { return f.conn, f.err }

type addrConn struct {
	net.Conn
	addr net.Addr
}

func (a addrConn) RemoteAddr() net.Addr { return a.addr }

func TestAccept(t *testing.T) {
	t.Run("listener error", func(t *testing.T) {
		boom := errors.New("closed")
		l := &Listener{Listener: &fakeListener{err: boom}, From: loopback}
		if _, err := l.Accept(); !errors.Is(err, boom) {
			t.Errorf("got %v, want %v", err, boom)
		}
	})
	for name, addr := range map[string]net.Addr{
		"not TCP":   &net.UnixAddr{Name: "/tmp/s", Net: "unix"},
		"no IP":     &net.TCPAddr{},
		"IPv4 in 6": &net.TCPAddr{IP: net.ParseIP("::ffff:127.0.0.1")},
	} {
		t.Run(name, func(t *testing.T) {
			a, b := net.Pipe()
			t.Cleanup(func() { _ = a.Close(); _ = b.Close() }) // the test is over
			l := &Listener{Listener: &fakeListener{conn: addrConn{Conn: a, addr: addr}}, From: loopback}
			c, err := l.Accept()
			if err != nil {
				t.Fatal(err)
			}
			_, wrapped := c.(*Conn)
			if want := name == "IPv4 in 6"; wrapped != want {
				t.Errorf("read for a header: got %v, want %v", wrapped, want)
			}
		})
	}
	t.Run("deadline fails", func(t *testing.T) {
		a, b := net.Pipe()
		_ = b.Close()
		_ = a.Close() // a closed pipe refuses deadlines
		c := &Conn{Conn: a, timeout: time.Second}
		if _, err := c.Read(make([]byte, 1)); err == nil || !strings.Contains(err.Error(), "header deadline") {
			t.Errorf("read: got %v, want the deadline's error", err)
		}
	})
}

func TestParseFrom(t *testing.T) {
	got, err := ParseFrom("ERP_X", " 10.1.0.0/20, 10.1.16.5/20,,")
	if err != nil {
		t.Fatal(err)
	}
	if want := []netip.Prefix{netip.MustParsePrefix("10.1.0.0/20"), netip.MustParsePrefix("10.1.16.0/20")}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v (masked)", got, want)
	}
	if got, err := ParseFrom("ERP_X", ""); err != nil || got != nil {
		t.Errorf("empty: got %v, %v, want none", got, err)
	}
	if _, err := ParseFrom("ERP_X", "10.0.0.0/33"); err == nil || !strings.Contains(err.Error(), `ERP_X must be a comma-separated list of CIDRs (the load balancer's subnets), not "10.0.0.0/33"`) {
		t.Errorf("bad CIDR: got %v", err)
	}
}

func TestListen(t *testing.T) {
	plain, err := Listen(t.Context(), "127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plain.Close() }() // the test is over
	if _, ok := plain.(*Listener); ok {
		t.Error("without subnets: got a PROXY listener, want the plain one")
	}
	behind, err := Listen(t.Context(), "127.0.0.1:0", loopback)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = behind.Close() }() // the test is over
	if l, ok := behind.(*Listener); !ok || !slices.Equal(l.From, loopback) {
		t.Errorf("with subnets: got %T, want a PROXY listener trusting them", behind)
	}
	if _, err := Listen(t.Context(), plain.Addr().String(), nil); err == nil || !strings.Contains(err.Error(), "proxyproto:") {
		t.Errorf("address in use: got %v", err)
	}
}
