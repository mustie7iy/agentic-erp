package globalplane

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf"
)

// TestUpdatedAndClient covers the version race the HTTP tests cannot stage
// and the injectable client.
func TestUpdatedAndClient(t *testing.T) {
	s := &Service{}
	if _, err := s.updated(directory.Entry{}, directory.ErrVersion); problem.From(err).Code != "request.version_mismatch" {
		t.Fatalf("version: %v", err)
	}
	if e, err := s.updated(directory.Entry{Cell: "x"}, nil); err != nil || e.Cell != "x" {
		t.Fatalf("%+v %v", e, err)
	}
	other := errors.New("other")
	// Any other error is passed on as it is: not mapped to a problem.
	_, err := s.updated(directory.Entry{}, other)
	var p *problem.Problem
	if !errors.Is(err, other) || errors.As(err, &p) {
		t.Fatalf("other: got %v, want it passed on unmapped", err)
	}
	if s.client().Timeout == 0 {
		t.Fatal("default client without a timeout")
	}
	own := &http.Client{}
	if (&Service{Client: own}).client() != own {
		t.Fatal("the service's client was not used")
	}
}

// TestCallRefusesPlaintextCells: with RequireTLS (production) a cell
// whose control URL is not https is not called: 503 (ADR-0033).
func TestCallRefusesPlaintextCells(t *testing.T) {
	s := &Service{RequireTLS: true}
	_, _, _, err := s.call(t.Context(), directory.Cell{Name: "eu-1", ControlURL: "http://eu1-admin.internal:9090"}, http.MethodGet, "/control/v1/x", nil, nil)
	if p := problem.From(err); p.Status != http.StatusServiceUnavailable {
		t.Errorf("plaintext control URL: got %v, want 503", err)
	}
}

// roundTrip is a transport made of a function.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestCallNamesTheCell: with PeerOf a call names the identity of the cell
// it is for, which alone the connection accepts (ADR-0033).
func TestCallNamesTheCell(t *testing.T) {
	var named string
	s := &Service{PeerOf: func(cell string) string { return "spiffe://erp.example/" + cell + "/erp-server" },
		Client: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			named, _ = tlsconf.ExpectedPeer(r.Context())
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}"))}, nil
		})}}
	if _, _, _, err := s.call(t.Context(), directory.Cell{Name: "eu-1", ControlURL: "https://eu1-admin.internal"}, http.MethodGet, "/control/v1/x", nil, nil); err != nil {
		t.Fatal(err)
	}
	if named != "spiffe://erp.example/eu-1/erp-server" {
		t.Errorf("named peer: got %q, want eu-1's", named)
	}
}
