package integration

import (
	"context"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/egress"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// wmsAnswering is the stand-in's adapter against a server answering every
// request with status and body.
func wmsAnswering(t *testing.T, status int, body string) *WMSStandIn {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body)) // the test's own answer; a client gone gets nothing
	}))
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	client, err := NewWMSClient(egress.LoopbackForTests(roots))
	if err != nil {
		t.Fatal(err)
	}
	return &WMSStandIn{Client: client, BaseURL: srv.URL, Token: "t"}
}

// TestWMSStandInAnswers: the adapter takes a 2xx as sent, a 4xx but 408
// and 429 as the WMS's refusal and anything else, or no answer, as a
// failed call; it refuses a list it cannot read and a confirmation,
// acknowledgement or list answered otherwise than as the protocol says.
func TestWMSStandInAnswers(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		status  int
		refusal bool
	}{
		"taken":           {http.StatusNoContent, false},
		"refused":         {http.StatusUnprocessableEntity, true},
		"a timeout":       {http.StatusRequestTimeout, false},
		"too many":        {http.StatusTooManyRequests, false},
		"a server error":  {http.StatusBadGateway, false},
		"a redirect seen": {http.StatusMultipleChoices, false},
	} {
		t.Run("send, "+name, func(t *testing.T) {
			err := wmsAnswering(t, c.status, "").Send(ctx, "M1", []byte(`{}`))
			switch {
			case c.status == http.StatusNoContent && err != nil:
				t.Errorf("got %v, want sent", err)
			case c.refusal && problem.From(err).Code != wmsRefused:
				t.Errorf("got %v, want %s", err, wmsRefused)
			case !c.refusal && c.status != http.StatusNoContent && (err == nil || problem.From(err).Status < 500):
				t.Errorf("got %v, want a failed call", err)
			}
		})
	}
	for name, c := range map[string]struct {
		status int
		body   string
	}{
		"another status": {http.StatusServiceUnavailable, `{}`},
		"not JSON":       {http.StatusOK, `{`},
	} {
		t.Run("list, "+name, func(t *testing.T) {
			if _, err := wmsAnswering(t, c.status, c.body).Confirmations(ctx); err == nil {
				t.Error("got nil, want the list refused")
			}
		})
	}
	if err := wmsAnswering(t, http.StatusOK, "").Acknowledge(ctx, "M1"); err != nil {
		t.Errorf("an acknowledgement answered 200: got %v, want taken", err)
	}
	if err := wmsAnswering(t, http.StatusNotFound, "").Acknowledge(ctx, "M1"); err == nil {
		t.Error("an acknowledgement answered 404: got nil, want refused")
	}
	if _, err := wmsAnswering(t, http.StatusNotFound, "").Confirmation(ctx, "M1"); err == nil {
		t.Error("a confirmation answered 404: got nil, want refused")
	}
	gone := wmsAnswering(t, http.StatusOK, "")
	gone.BaseURL = "https://127.0.0.1:1"
	if err := gone.Send(ctx, "M1", nil); err == nil {
		t.Error("send to no server: got nil, want a failed call")
	}
	if _, err := gone.Confirmations(ctx); err == nil {
		t.Error("list from no server: got nil, want a failed call")
	}
	if _, err := gone.Confirmation(ctx, "M1"); err == nil {
		t.Error("read from no server: got nil, want a failed call")
	}
	if err := gone.Acknowledge(ctx, "M1"); err == nil {
		t.Error("acknowledge to no server: got nil, want a failed call")
	}
}
