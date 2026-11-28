package workflowtest

import (
	"errors"
	"fmt"
	"net"
	"os/exec"
	"testing"
)

// TestServerOnATakenPort: a server whose first port is taken, as another
// process can take the port freePort found before the server binds it,
// starts on another (review:batch96; PR #150's race shard).
func TestServerOnATakenPort(t *testing.T) {
	if _, err := exec.LookPath("temporal"); err != nil {
		t.Skip("temporal CLI not on the path")
	}
	l, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }() // the port only needs to be held while the server starts
	taken := l.Addr().(*net.TCPAddr).Port
	tried := 0
	port = func(tb testing.TB) int {
		tb.Helper()
		tried++
		if tried == 1 {
			return taken
		}
		return freePort(tb)
	}
	t.Cleanup(func() { port = freePort })
	if got := Server(t); got == fmt.Sprintf("127.0.0.1:%d", taken) {
		t.Errorf("address: got %s, want another port than the taken one", got)
	}
	if tried != 2 {
		t.Errorf("ports tried: got %d, want 2", tried)
	}
}

// TestServerOnAnotherBinarysServer: a port another test binary's server
// took answers a dial too; without this binary's namespace it is not
// taken for this binary's, which starts on another port.
func TestServerOnAnotherBinarysServer(t *testing.T) {
	bin, err := exec.LookPath("temporal")
	if err != nil {
		t.Skip("temporal CLI not on the path")
	}
	taken := freePort(t)
	other, halt, err := startOn(bin, taken)
	if err != nil {
		t.Fatalf("the other binary's server: %v", err)
	}
	defer halt()
	if err := ours(other, "workflowtest-elsewhere"); !errors.Is(err, errPortTaken) {
		t.Errorf("another namespace: got %v, want the server taken as another's", err)
	}
	tried := 0
	port = func(tb testing.TB) int {
		tb.Helper()
		tried++
		if tried == 1 {
			return taken
		}
		return freePort(tb)
	}
	t.Cleanup(func() { port = freePort })
	if got := Server(t); got == other {
		t.Errorf("address: got %s, the other binary's server, want one of its own", got)
	}
	if tried != 2 {
		t.Errorf("ports tried: got %d, want 2", tried)
	}
}

// TestServerSkippedGivesItsUseBack: a test skipped for want of the CLI
// leaves no use counted, so the next test's server is started and
// stopped as the only one.
func TestServerSkippedGivesItsUseBack(t *testing.T) {
	t.Run("without the CLI", func(t *testing.T) {
		t.Setenv("PATH", "")
		Server(t)
		t.Error("got no skip, want the test skipped without the CLI")
	})
	mu.Lock()
	defer mu.Unlock()
	if users != 0 {
		t.Errorf("uses after the skipped test: got %d, want 0", users)
	}
}
