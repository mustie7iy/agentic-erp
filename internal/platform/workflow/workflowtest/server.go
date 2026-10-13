// Package workflowtest starts a Temporal development server for tests.
package workflowtest

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
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
	port := freePort(tb)
	// Not tb.Context(): the server is shared and outlives the test that starts
	// it; release stops it when the last user ends.
	cmd := exec.CommandContext(context.Background(), bin, "server", "start-dev", "--headless", "--ip", "127.0.0.1", "--port", strconv.Itoa(port), "--metrics-port", "0", "--log-level", "error") // #nosec G204 -- the CLI found on the path, fixed arguments
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		tb.Fatalf("workflowtest: %v", err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(60 * time.Second)
	for {
		c, err := client.DialContext(context.Background(), client.Options{HostPort: addr})
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			tb.Fatalf("workflowtest: the development server did not come up: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	address = addr
	stop = func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }
	return address
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
	defer func() { _ = l.Close() }() // it only finds a free port; were the port still bound, the server start would fail and say so
	return l.Addr().(*net.TCPAddr).Port
}
