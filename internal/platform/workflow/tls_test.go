package workflow_test

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf/tlstest"
	"github.com/mustie7iy/agentic-erp/internal/platform/workflow"
	"github.com/mustie7iy/agentic-erp/internal/platform/workflow/workflowtest"
)

// TestDialOverMutualTLS: with TLS the engine is reached over TLS 1.3,
// presenting the client's certificate and verifying the frontend's
// (ADR-0033); each connection takes a fresh configuration, and a frontend
// the roots do not trust, or a configuration that cannot be made, is
// refused rather than dialled in plaintext.
func TestDialOverMutualTLS(t *testing.T) {
	ca := tlstest.NewCA(t)
	// The frontend's certificate names it, not its address: the
	// configuration's server name is what is verified.
	frontend := tlstest.Proxy(t, workflowtest.Server(t), ca.Issue(t, tlstest.Options{DNS: []string{"temporal.internal"}}).Pair(t), ca.Pool(), "h2")
	cell := ca.Issue(t, tlstest.Options{}).Pair(t)
	var configs atomic.Int32 // taken on gRPC's goroutines
	config := func(roots *tlstest.CA) func() (*tls.Config, error) {
		return func() (*tls.Config, error) {
			configs.Add(1)
			return &tls.Config{MinVersion: tls.VersionTLS13, ServerName: "temporal.internal", RootCAs: roots.Pool(), Certificates: []tls.Certificate{cell}}, nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	engine, err := workflow.Dial(ctx, workflow.Config{Address: frontend, Namespace: "default", TLS: config(ca)})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if err := engine.Cancel(ctx, "no-such-workflow"); err != nil {
		t.Errorf("a call over TLS: got %v, want served", err)
	}
	if configs.Load() == 0 {
		t.Error("configurations: got none taken, want one per connection")
	}
	for _, c := range []struct {
		name string
		tls  func() (*tls.Config, error)
		want string
	}{
		{"untrusted frontend", config(tlstest.NewCA(t)), "certificate"},
		{"no configuration", func() (*tls.Config, error) { return nil, errors.New("material not read yet") }, "material not read yet"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := workflow.Dial(ctx, workflow.Config{Address: frontend, Namespace: "default", TLS: c.tls}); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("dial: got %v, want refused with %q", err, c.want)
			}
		})
	}
}
