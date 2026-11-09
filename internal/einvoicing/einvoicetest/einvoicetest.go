// Package einvoicetest is the e-invoicing provider's stand-in (ADR-0061
// §3): an HTTPS server speaking the stand-in adapter's protocol
// (einvoicing.StandIn), for tests (Server, on loopback with its own
// certificate) and for the walkthrough (providerstub, over a directory).
// It answers 401 to a request without the bearer token it was given.
package einvoicetest

import (
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Message is a document the stand-in holds.
type Message struct {
	ID, Receiver string
	Body         []byte
}

// Store is what the stand-in serves: its messages not yet acknowledged,
// one's bytes, and an acknowledgement (false when there is no such
// message).
type Store interface {
	Pending() ([]Message, error)
	Get(id string) ([]byte, bool, error)
	Acknowledge(id string) (bool, error)
}

// Handler serves store under the protocol's paths, to requests bearing
// token.
func Handler(store Store, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /messages", func(w http.ResponseWriter, _ *http.Request) {
		ms, err := store.Pending()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		type listed struct {
			ID       string `json:"id"`
			Receiver string `json:"receiver,omitempty"`
		}
		out := struct {
			Messages []listed `json:"messages"`
		}{Messages: []listed{}}
		for _, m := range ms {
			out.Messages = append(out.Messages, listed{ID: m.ID, Receiver: m.Receiver})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out) // a client that went away gets nothing; the stand-in does not care
	})
	mux.HandleFunc("GET /messages/{id}", func(w http.ResponseWriter, r *http.Request) {
		body, ok, err := store.Get(r.PathValue("id"))
		switch {
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		case !ok:
			http.NotFound(w, r)
		default:
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write(body) // #nosec G705 -- the test double serves the document it was given, as XML, to the ERP alone; as above, a client gone gets nothing
		}
	})
	mux.HandleFunc("POST /messages/{id}/acknowledge", func(w http.ResponseWriter, r *http.Request) {
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

// Server is the stand-in for a test: messages held in memory, its
// acknowledgements counted, and answers it can be told to fail.
type Server struct {
	URL   string         // the base URL, https on loopback
	Roots *x509.CertPool // its certificate, for egress.LoopbackForTests

	mu       sync.Mutex
	messages []Message
	acked    map[string]int
	fail     map[string]int // path -> answers still to fail with 503
}

// NewServer starts a stand-in serving requests bearing token, closed when
// the test ends.
func NewServer(t *testing.T, token string) *Server {
	t.Helper()
	s := &Server{acked: map[string]int{}, fail: map[string]int{}}
	srv := httptest.NewTLSServer(s.failing(Handler(s, token)))
	t.Cleanup(srv.Close)
	s.URL = srv.URL + "/provider"
	s.Roots = x509.NewCertPool()
	s.Roots.AddCert(srv.Certificate())
	return s
}

// failing answers 503 to a path while Fail says to, then serves next.
func (s *Server) failing(next http.Handler) http.Handler {
	return http.StripPrefix("/provider", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		key := r.Method + " " + r.URL.Path
		fail := s.fail[key] > 0
		if fail {
			s.fail[key]--
		}
		s.mu.Unlock()
		if fail {
			http.Error(w, "failing", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// Deliver makes a message pending at the stand-in.
func (s *Server) Deliver(id, receiver string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, Message{ID: id, Receiver: receiver, Body: body})
}

// Redeliver makes a message delivered before pending again with body, as
// a provider delivering other bytes under the same id (ADR-0061 §4).
func (s *Server) Redeliver(id string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.messages {
		if s.messages[i].ID == id {
			s.messages[i].Body = body
		}
	}
	delete(s.acked, id)
}

// Fail makes the next times answers to the method and path (below the
// base URL, such as "GET /messages") fail with 503.
func (s *Server) Fail(methodPath string, times int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[methodPath] = times
}

// Acknowledged is how many times the message was acknowledged.
func (s *Server) Acknowledged(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acked[id]
}

// Pending implements Store: the messages never acknowledged.
func (s *Server) Pending() ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(s.messages), func(m Message) bool { return s.acked[m.ID] > 0 }), nil
}

// Get implements Store.
func (s *Server) Get(id string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.messages {
		if m.ID == id {
			return m.Body, true, nil
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

// Invoice is a minimal document for tests: what T65a records is the bytes,
// not what they say (T65c reads them).
func Invoice(number string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<Invoice xmlns="urn:oasis:names:specification:ubl:schema:xsd:Invoice-2"><ID>` +
		strings.ReplaceAll(number, "<", "") + `</ID></Invoice>` + "\n")
}
