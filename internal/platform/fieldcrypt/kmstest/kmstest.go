// Package kmstest is a test double of AWS KMS: one symmetric key behind the
// Encrypt and Decrypt actions of the JSON protocol, checking Signature
// Version 4 and binding every ciphertext to its encryption context, so the
// KMS master can be tested without the service.
package kmstest

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/sigv4"
)

// Credentials and key the server accepts.
const (
	AccessKey = "AKIDEXAMPLE"
	SecretKey = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY" // #nosec G101 -- the AWS documentation's example key, for a test double
	Region    = "us-east-1"
	KeyID     = "alias/erp-master"
	KeyARN    = "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
)

// Server is the running double.
type Server struct {
	URL  string
	aead cipher.AEAD
	mu   sync.Mutex
	// Encrypts and Decrypts count the successful calls; read them through
	// Calls while the server serves.
	Encrypts, Decrypts int
	// Fail makes every call answer this status (0: serve normally) with the
	// exception FailType (default KMSInternalException); FailPlain answers
	// with a non-JSON body.
	Fail      int
	FailType  string
	FailPlain bool
}

// New starts a server that stops when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	key := make([]byte, 32)
	rand.Read(key)
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	s := &Server{aead: aead}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

// Master is a KMS master pointed at the server.
func (s *Server) Master(name string) fieldcrypt.KMS {
	return fieldcrypt.KMS{Name: name, KeyID: KeyID, Endpoint: s.URL, Region: Region, AccessKey: AccessKey, SecretKey: SecretKey}
}

// Calls returns the successful Encrypt and Decrypt counts.
func (s *Server) Calls() (encrypts, decrypts int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Encrypts, s.Decrypts
}

type request struct {
	KeyID             string            `json:"KeyId"`
	Plaintext         []byte            `json:"Plaintext"`
	CiphertextBlob    []byte            `json:"CiphertextBlob"`
	EncryptionContext map[string]string `json:"EncryptionContext"`
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	if !s.authentic(r, body) {
		s.fail(w, http.StatusForbidden, "IncompleteSignatureException", "The request signature does not match.")
		return
	}
	if s.Fail != 0 {
		if s.FailPlain {
			w.WriteHeader(s.Fail)
			w.Write([]byte("gateway says no"))
			return
		}
		typ := s.FailType
		if typ == "" {
			typ = "KMSInternalException"
		}
		s.fail(w, s.Fail, typ, "An internal error occurred.")
		return
	}
	var in request
	if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-amz-json-1.1" || json.Unmarshal(body, &in) != nil {
		s.fail(w, http.StatusBadRequest, "SerializationException", "The request is not JSON 1.1.")
		return
	}
	if in.KeyID != KeyID && in.KeyID != KeyARN {
		s.fail(w, http.StatusBadRequest, "NotFoundException", "Key '"+in.KeyID+"' does not exist")
		return
	}
	s.operate(w, r, in, []byte(contextOf(in.EncryptionContext)))
}

// operate runs the operation the request targets (Encrypt or Decrypt)
// under the encryption context aad, the lock held.
func (s *Server) operate(w http.ResponseWriter, r *http.Request, in request, aad []byte) {
	switch r.Header.Get("X-Amz-Target") {
	case "TrentService.Encrypt":
		nonce := make([]byte, s.aead.NonceSize())
		rand.Read(nonce)
		s.Encrypts++
		s.answer(w, map[string]any{"CiphertextBlob": s.aead.Seal(nonce, nonce, in.Plaintext, aad), "KeyId": KeyARN})
	case "TrentService.Decrypt":
		n := s.aead.NonceSize()
		if len(in.CiphertextBlob) < n {
			s.fail(w, http.StatusBadRequest, "InvalidCiphertextException", "")
			return
		}
		plain, err := s.aead.Open(nil, in.CiphertextBlob[:n], in.CiphertextBlob[n:], aad)
		if err != nil {
			s.fail(w, http.StatusBadRequest, "InvalidCiphertextException", "")
			return
		}
		s.Decrypts++
		s.answer(w, map[string]any{"Plaintext": plain, "KeyId": KeyARN})
	default:
		s.fail(w, http.StatusBadRequest, "UnknownOperationException", "")
	}
}

// contextOf canonicalizes an encryption context: sorted key=value pairs.
func contextOf(ctx map[string]string) string {
	keys := make([]string, 0, len(ctx))
	for k := range ctx {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+ctx[k])
	}
	return strings.Join(parts, "&")
}

// authentic recomputes the signature with the server's credentials at the
// time the request names, and compares.
func (s *Server) authentic(r *http.Request, body []byte) bool {
	got := r.Header.Get("Authorization")
	when, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil || got == "" {
		return false
	}
	clone := r.Clone(r.Context())
	clone.Header.Del("Authorization")
	sigv4.Sign(clone, AccessKey, SecretKey, Region, "kms", sigv4.PayloadHash(body), when)
	return clone.Header.Get("Authorization") == got
}

func (s *Server) answer(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) fail(w http.ResponseWriter, status int, typ, message string) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"__type": typ, "message": message})
}
