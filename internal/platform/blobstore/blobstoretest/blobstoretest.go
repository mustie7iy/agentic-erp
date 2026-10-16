// Package blobstoretest is a test double of an S3-compatible service: an
// in-memory bucket over HTTP that checks Signature Version 4, honours
// If-None-Match: * and records object-lock headers, so the write-once store
// of ADR-0016 can be tested without a service (06-engineering-process §6.6).
package blobstoretest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/sigv4"
)

// Credentials the server accepts.
const (
	AccessKey = "AKIDEXAMPLE"
	SecretKey = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY" // #nosec G101 -- the AWS documentation's example key, for a test double
	Region    = "us-east-1"
	Bucket    = "erp-test"
)

// Object is a stored object.
type Object struct {
	Content     []byte
	ContentType string
	RetainUntil string // the x-amz-object-lock-retain-until-date header, "" without lock
	LockMode    string
	Encryption  string // the x-amz-server-side-encryption header, "" without
	KeyID       string // the x-amz-server-side-encryption-aws-kms-key-id header
}

// Server is the running double.
type Server struct {
	URL string
	mu  sync.Mutex
	// Objects by key; read it through Object or Keys while the server
	// serves, since handlers write it concurrently.
	Objects map[string]Object
	// Fail makes every call answer this status with an S3 error document
	// (0: serve normally); FailPlain answers it with a non-XML body.
	Fail      int
	FailPlain bool
	// Now is the clock presigned URLs and object locks are checked against
	// (nil: time.Now).
	Now func() time.Time
	// PageSize is how many keys a listing page holds (default 1000).
	PageSize int
	// BucketEncryption is the bucket's default encryption algorithm that
	// GetBucketEncryption reports ("": none configured).
	BucketEncryption string
	// BucketKeyID is the KMS key of that default ("": the AWS managed key).
	BucketKeyID string
}

// New starts a server that stops when the test ends.
func New(tb testing.TB) *Server {
	tb.Helper()
	s := &Server{Objects: map[string]Object{}}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	tb.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

// Object returns a stored object.
func (s *Server) Object(key string) (Object, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.Objects[key]
	return o, ok
}

// Keys lists the stored keys.
func (s *Server) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.Objects))
	for k := range s.Objects {
		keys = append(keys, k)
	}
	return keys
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
	if r.URL.Path == "/"+Bucket && r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
		s.list(w, r)
		return
	}
	if r.URL.Path == "/"+Bucket && r.Method == http.MethodGet && r.URL.Query().Has("encryption") {
		s.encryption(w)
		return
	}
	key, ok := strings.CutPrefix(r.URL.Path, "/"+Bucket+"/")
	if !ok {
		s.answer(w, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist.")
		return
	}
	s.object(w, r, key, body)
}

// object answers a request for one object (delete, head, put, get), the
// lock held.
func (s *Server) object(w http.ResponseWriter, r *http.Request, key string, body []byte) {
	switch r.Method {
	case http.MethodDelete:
		if o, exists := s.Objects[key]; exists {
			if until, err := time.Parse(time.RFC3339, o.RetainUntil); err == nil && s.now().Before(until) {
				s.answer(w, http.StatusForbidden, "AccessDenied", "Access Denied because object protected by object lock.")
				return
			}
			delete(s.Objects, key)
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodHead:
		o, exists := s.Objects[key]
		if !exists {
			s.answer(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
			return
		}
		w.Header().Set("Content-Type", o.ContentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(o.Content)))
		w.WriteHeader(http.StatusOK)
	case http.MethodPut:
		if _, exists := s.Objects[key]; exists && r.Header.Get("If-None-Match") == "*" {
			s.answer(w, http.StatusPreconditionFailed, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold.")
			return
		}
		s.Objects[key] = Object{Content: body, ContentType: r.Header.Get("Content-Type"), RetainUntil: r.Header.Get("x-amz-object-lock-retain-until-date"),
			LockMode: r.Header.Get("x-amz-object-lock-mode"), Encryption: r.Header.Get("x-amz-server-side-encryption"),
			KeyID: r.Header.Get("x-amz-server-side-encryption-aws-kms-key-id")}
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
// credentials, at the time the request names, and compares. A presigned
// request (signature in the query) must also be within its validity at
// Now (the clock of the test, default the real one).
func (s *Server) authentic(r *http.Request, body []byte) bool {
	if r.URL.Query().Get("X-Amz-Signature") != "" {
		ok, made, ttl := sigv4.Verify(r, AccessKey, SecretKey, Region, "s3")
		return ok && s.now().Before(made.Add(ttl))
	}
	got := r.Header.Get("Authorization")
	when, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil || got == "" {
		return false
	}
	clone := r.Clone(r.Context())
	clone.Header.Del("Authorization")
	sigv4.Sign(clone, AccessKey, SecretKey, Region, "s3", r.Header.Get("X-Amz-Content-Sha256"), when)
	_ = body
	return clone.Header.Get("Authorization") == got
}

func (s *Server) now() time.Time {
	return clock.Now(s.Now)
}

// list answers ListObjectsV2: the keys with the prefix, sorted, PageSize
// (default 1000) per page with a continuation token.
func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	prefix, token := r.URL.Query().Get("prefix"), r.URL.Query().Get("continuation-token")
	var keys []string
	for k := range s.Objects {
		if strings.HasPrefix(k, prefix) && k > token {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	size := s.PageSize
	if size <= 0 {
		size = 1000
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult>`)
	for i, k := range keys {
		if i == size {
			b.WriteString("<IsTruncated>true</IsTruncated><NextContinuationToken>" + keys[i-1] + "</NextContinuationToken>")
			break
		}
		b.WriteString("<Contents><Key>" + k + "</Key></Contents>")
	}
	b.WriteString("</ListBucketResult>")
	w.Header().Set("Content-Type", "application/xml")
	w.Write([]byte(b.String()))
}

// encryption answers GetBucketEncryption with BucketEncryption, or S3's
// error when the bucket has none.
func (s *Server) encryption(w http.ResponseWriter) {
	if s.BucketEncryption == "" {
		s.answer(w, http.StatusNotFound, "ServerSideEncryptionConfigurationNotFoundError", "The server side encryption configuration was not found")
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><ServerSideEncryptionConfiguration><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>` +
		s.BucketEncryption + `</SSEAlgorithm><KMSMasterKeyID>` + s.BucketKeyID + `</KMSMasterKeyID></ApplyServerSideEncryptionByDefault></Rule></ServerSideEncryptionConfiguration>`))
}

func (s *Server) answer(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>` + code + `</Code><Message>` + message + `</Message></Error>`))
}
