// Package wmstest is the warehouse system's stand-in (ADR-0082 As built
// S2q): an HTTPS server speaking the stand-in adapter's protocol
// (integration.WMSStandIn), for tests (Server, on loopback with its own
// certificate, in memory) and for the walkthrough (wmsstub, over a
// directory). It answers 401 to a request without the bearer token it
// was given. It is test support and is excluded from the coverage figure.
package wmstest

import (
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// maxBody bounds what the stand-in reads of a message sent to it.
const maxBody = 1 << 20

// Store is what the stand-in keeps: the messages sent to it by id, and the
// confirmations it reports, not yet acknowledged.
type Store interface {
	// Receive keeps a message; one held already under its id is kept as it
	// was (the WMS deduplicates on the id).
	Receive(id string, body []byte) error
	Pending() ([]string, error)
	Get(id string) ([]byte, bool, error)
	Acknowledge(id string) (bool, error)
}

// Handler serves store under the protocol's paths, to requests bearing
// token.
func Handler(store Store, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /messages/{id}", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil || !json.Valid(body) {
			http.Error(w, "a JSON message", http.StatusBadRequest)
			return
		}
		if err := store.Receive(r.PathValue("id"), body); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /confirmations", func(w http.ResponseWriter, _ *http.Request) {
		ids, err := store.Pending()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		type listed struct {
			ID string `json:"id"`
		}
		out := struct {
			Confirmations []listed `json:"confirmations"`
		}{Confirmations: []listed{}}
		for _, id := range ids {
			out.Confirmations = append(out.Confirmations, listed{ID: id})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out) // a client that went away gets nothing; the stand-in does not care
	})
	mux.HandleFunc("GET /confirmations/{id}", func(w http.ResponseWriter, r *http.Request) {
		body, ok, err := store.Get(r.PathValue("id"))
		switch {
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		case !ok:
			http.NotFound(w, r)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body) // #nosec G705 -- the test double serves the confirmation it was given, as JSON, to the ERP alone; a client gone gets nothing
		}
	})
	mux.HandleFunc("POST /confirmations/{id}/acknowledge", func(w http.ResponseWriter, r *http.Request) {
		ok, err := store.Acknowledge(r.PathValue("id"))
		switch {
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		case !ok:
			http.NotFound(w, r)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// Server is the stand-in for a test: what it is sent and what it reports
// held in memory, its acknowledgements counted, and answers it can be
// told to fail.
type Server struct {
	URL   string         // the protocol's base URL, https on loopback
	Roots *x509.CertPool // its certificate, for egress.LoopbackForTests

	mu       sync.Mutex
	received map[string][]byte
	sends    map[string]int
	reports  []report
	acked    map[string]int
	fail     map[string]int // method and path -> answers still to fail
	status   int            // the status failing answers carry
}

type report struct {
	id   string
	body []byte
}

// NewServer starts a stand-in serving requests bearing token, closed when
// the test ends.
func NewServer(t *testing.T, token string) *Server {
	t.Helper()
	s := &Server{received: map[string][]byte{}, sends: map[string]int{}, acked: map[string]int{}, fail: map[string]int{}, status: http.StatusServiceUnavailable}
	srv := httptest.NewTLSServer(s.failing(Handler(s, token)))
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	s.Roots = x509.NewCertPool()
	s.Roots.AddCert(srv.Certificate())
	return s
}

// failing answers the failing status to a method and path while Fail
// says to, then serves next.
func (s *Server) failing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		key := r.Method + " " + r.URL.Path
		if strings.HasPrefix(key, "PUT /messages/") {
			key = "PUT /messages"
		}
		fail, status := s.fail[key] > 0, s.status
		if fail {
			s.fail[key]--
		}
		s.mu.Unlock()
		if fail {
			http.Error(w, "failing", status)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Fail makes the next times answers to the method and path ("PUT
// /messages" for any message, "GET /confirmations", ...) fail with status.
func (s *Server) Fail(methodPath string, times, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[methodPath], s.status = times, status
}

// Report makes a confirmation pending at the stand-in, or pending again
// with body under an id reported before.
func (s *Server) Report(id string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reports = slices.DeleteFunc(s.reports, func(r report) bool { return r.id == id })
	s.reports = append(s.reports, report{id: id, body: body})
	delete(s.acked, id)
}

// Received is the message the stand-in holds under id, and how many times
// it was sent.
func (s *Server) Received(id string) ([]byte, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.received[id], s.sends[id]
}

// Acknowledged is how many times the confirmation was acknowledged.
func (s *Server) Acknowledged(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acked[id]
}

// Receive implements Store.
func (s *Server) Receive(id string, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.received[id]; !ok {
		s.received[id] = body
	}
	s.sends[id]++
	return nil
}

// Pending implements Store: the confirmations never acknowledged.
func (s *Server) Pending() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.reports {
		if s.acked[r.id] == 0 {
			out = append(out, r.id)
		}
	}
	return out, nil
}

// Get implements Store.
func (s *Server) Get(id string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.reports {
		if r.id == id {
			return r.body, true, nil
		}
	}
	return nil, false, nil
}

// Acknowledge implements Store.
func (s *Server) Acknowledge(id string) (bool, error) {
	if _, ok, _ := s.Get(id); !ok { // the memory store's Get cannot fail
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acked[id]++
	return true, nil
}
