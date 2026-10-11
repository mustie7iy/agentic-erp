// Package blobstoretest is a test double of an S3-compatible service: an
// in-memory bucket over HTTP that checks Signature Version 4, honours
// If-None-Match: * and records object-lock headers, so S3 can be tested
// without a service.
package blobstoretest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
)

// Credentials the server accepts.
const (
	AccessKey = "AKIDEXAMPLE"
	SecretKey = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	Region    = "us-east-1"
	Bucket    = "erp-test"
)

// Object is a stored object.
type Object struct {
	Content     []byte
	ContentType string
	RetainUntil string // the x-amz-object-lock-retain-until-date header, "" without lock
	LockMode    string
}

// Server is the running double.
type Server struct {
	URL string
	mu  sync.Mutex
	// Objects by key.
	Objects map[string]Object
	// Fail makes every call answer this status with an S3 error document
	// (0: serve normally); FailPlain answers it with a non-XML body.
	Fail      int
	FailPlain bool
}

// New starts a server that stops when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{Objects: map[string]Object{}}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

// Store is an S3 store pointed at the server.
func (s *Server) Store() blobstore.S3 {
	return blobstore.S3{Endpoint: s.URL, Bucket: Bucket, Region: Region, AccessKey: AccessKey, SecretKey: SecretKey}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	if !s.authentic(r, body) {
		s.answer(w, http.StatusForbidden, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided.")
		return
	}
	if s.Fail != 0 {
		if s.FailPlain {
			w.WriteHeader(s.Fail)
			w.Write([]byte("gateway says no"))
			return
		}
		s.answer(w, s.Fail, "InternalError", "We encountered an internal error. Please try again.")
		return
	}
	key, ok := strings.CutPrefix(r.URL.Path, "/"+Bucket+"/")
	if !ok {
		s.answer(w, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist.")
		return
	}
	switch r.Method {
	case http.MethodPut:
		if _, exists := s.Objects[key]; exists && r.Header.Get("If-None-Match") == "*" {
			s.answer(w, http.StatusPreconditionFailed, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold.")
			return
		}
		s.Objects[key] = Object{Content: body, ContentType: r.Header.Get("Content-Type"), RetainUntil: r.Header.Get("x-amz-object-lock-retain-until-date"),
			LockMode: r.Header.Get("x-amz-object-lock-mode")}
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		o, exists := s.Objects[key]
		if !exists {
			s.answer(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
			return
		}
		if o.ContentType != "" {
			w.Header().Set("Content-Type", o.ContentType)
		}
		w.Write(o.Content)
	default:
		s.answer(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed against this resource.")
	}
}

// authentic recomputes the signature of the request with the server's
// credentials, at the time the request names, and compares.
func (s *Server) authentic(r *http.Request, body []byte) bool {
	got := r.Header.Get("Authorization")
	when, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil || got == "" {
		return false
	}
	clone := r.Clone(r.Context())
	clone.Header.Del("Authorization")
	blobstore.SignV4(clone, AccessKey, SecretKey, Region, "s3", r.Header.Get("X-Amz-Content-Sha256"), when)
	_ = body
	return clone.Header.Get("Authorization") == got
}

func (s *Server) answer(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>` + code + `</Code><Message>` + message + `</Message></Error>`))
}
