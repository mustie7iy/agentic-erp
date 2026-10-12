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
	if _, err := s.updated(directory.Entry{}, other); err != other {
		t.Fatalf("other: %v", err)
	}
	if s.client().Timeout == 0 {
		t.Fatal("default client without a timeout")
	}
	own := &http.Client{}
	if (&Service{Client: own}).client() != own {
		t.Fatal("the service's client was not used")
	}
}
