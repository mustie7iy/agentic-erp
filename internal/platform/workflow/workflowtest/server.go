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
func Server(t testing.TB) string {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	t.Cleanup(release)
	users++
	if address != "" {
		return address
	}
	bin, err := exec.LookPath("temporal")
	if err != nil {
		users--
		t.Skip("temporal CLI not on the path: brew install temporal, or https://temporal.download/cli.sh")
	}
	port := freePort(t)
	cmd := exec.Command(bin, "server", "start-dev", "--headless", "--ip", "127.0.0.1", "--port", strconv.Itoa(port), "--metrics-port", "0", "--log-level", "error") // #nosec G204 -- the CLI found on the path, fixed arguments
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("workflowtest: %v", err)
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
			t.Fatalf("workflowtest: the development server did not come up: %v", err)
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

func freePort(t testing.TB) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
