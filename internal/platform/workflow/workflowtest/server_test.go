package workflowtest

import (
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
