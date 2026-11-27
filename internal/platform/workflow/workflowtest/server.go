// Package workflowtest starts a Temporal development server for tests of the
// workflows of ADR-0012 (06-engineering-process §6.6).
package workflowtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
)

var (
	mu      sync.Mutex
	users   int
	address string
	stop    func()
)

// port picks the port a start tries: freePort, which a test replaces to
// hand out a port that is taken.
var port = freePort

// starts is how many times a start is tried on a new port when the server
// exits because its port was taken.
const starts = 3

// Server starts a headless development server (the `temporal` CLI must be
// on the path; the test is skipped otherwise) and returns its address. One
// server serves every test of the binary that asks while any of them runs;
// it stops when the last one ends. Its state is in memory.
func Server(tb testing.TB) string {
	tb.Helper()
	mu.Lock()
	defer mu.Unlock()
	tb.Cleanup(release)
	users++
	if address != "" {
		return address
	}
	bin, err := exec.LookPath("temporal")
	if err != nil {
		users--
		tb.Skip("temporal CLI not on the path: brew install temporal, or https://temporal.download/cli.sh")
	}
	address, stop = start(tb, bin)
	return address
}

// errPortTaken is a server that exited because its port was bound.
var errPortTaken = errors.New("its port was taken")

// start starts the server, on another free port when it exits because the
// one it was given was taken: freePort closes its listener before the
// server binds the port, so another process can take it meanwhile (PR
// #150's race shard failed once on "address already in use").
func start(tb testing.TB, bin string) (string, func()) {
	tb.Helper()
	for n := 1; ; n++ {
		addr, halt, err := startOn(bin, port(tb))
		if err == nil {
			return addr, halt
		}
		if !errors.Is(err, errPortTaken) || n == starts {
			tb.Fatalf("workflowtest: %v", err)
		}
	}
}

// startOn starts the server on the port and waits until a client can dial
// it, every 200 ms for a minute at most, or until it exits.
func startOn(bin string, port int) (string, func(), error) {
	var out syncBuffer // what it says, to tell a taken port from another failure
	// Not tb.Context(): the server is shared and outlives the test that starts
	// it; release stops it when the last user ends.
	cmd := exec.CommandContext(context.Background(), bin, "server", "start-dev", "--headless", "--ip", "127.0.0.1", "--port", strconv.Itoa(port), "--metrics-port", "0", "--log-level", "error") // #nosec G204 -- the CLI found on the path, fixed arguments
	cmd.Stdout, cmd.Stderr = os.Stderr, io.MultiWriter(os.Stderr, &out)
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("starting %s: %w", bin, err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }() // its exit is what matters here; dial reports it
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if err := dial(addr, exited); err != nil {
		_ = cmd.Process.Kill() // it may have exited already; the error says why it is given up
		<-exited
		if strings.Contains(out.String(), "address already in use") {
			return "", nil, fmt.Errorf("the development server on %s: %w", addr, errPortTaken)
		}
		return "", nil, fmt.Errorf("the development server did not come up: %w", err)
	}
	return addr, func() { _ = cmd.Process.Kill(); <-exited }, nil
}

// dial waits until a client dials addr, or the server exits.
func dial(addr string, exited <-chan struct{}) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		c, err := client.DialContext(ctx, client.Options{HostPort: addr})
		if err == nil {
			c.Close()
			return nil
		}
		select {
		case <-exited:
			return fmt.Errorf("it exited while dialing %s: %w", addr, err)
		case <-ctx.Done():
			return fmt.Errorf("dialing %s: %w", addr, err)
		case <-tick.C:
		}
	}
}

// syncBuffer is a buffer the server's output is copied into while it runs.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, _ := b.buf.Write(p) // a bytes.Buffer's Write returns no error: it panics on a buffer too large
	return n, nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// release is a test's cleanup: the last user stops the server.
func release() {
	mu.Lock()
	defer mu.Unlock()
	if users--; users == 0 && stop != nil {
		stop()
		stop, address = nil, ""
	}
}

func freePort(tb testing.TB) int {
	tb.Helper()
	l, err := new(net.ListenConfig).Listen(tb.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = l.Close() }() // it only finds a free port; were the port still bound, the server would exit and start tries another
	return l.Addr().(*net.TCPAddr).Port
}
