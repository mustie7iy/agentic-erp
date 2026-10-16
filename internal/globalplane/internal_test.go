package globalplane

import (
	"errors"
	"net/http"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
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
