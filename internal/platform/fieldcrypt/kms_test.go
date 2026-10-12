package fieldcrypt_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/awscreds"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt/kmstest"
)

func TestKMSMaster(t *testing.T) {
	srv := kmstest.New(t)
	m := srv.Master("")
	if m.ID() != "kms" || srv.Master("hsm1").ID() != "hsm1" {
		t.Fatal("master ids")
	}
	tenant := uuid.New()
	key := []byte(strings.Repeat("k", 32))
	wrapped, err := m.Wrap(tenant, key)
	if err != nil || len(wrapped) < 32 {
		t.Fatalf("%v %v", wrapped, err)
	}
	if got, err := m.Unwrap(tenant, wrapped); err != nil || string(got) != string(key) {
		t.Fatalf("%q %v", got, err)
	}
	if e, d := srv.Calls(); e != 1 || d != 1 {
		t.Fatalf("calls: %d %d", e, d)
	}
	// Bound to the tenant by the encryption context; tampered and truncated blobs are corrupt, not failures.
	for name, bad := range map[string]struct {
		tenant uuid.UUID
		blob   []byte
	}{
		"another tenant": {uuid.New(), wrapped},
		"tampered":       {tenant, append(append([]byte(nil), wrapped[:len(wrapped)-1]...), wrapped[len(wrapped)-1]^1)},
		"truncated":      {tenant, wrapped[:3]},
	} {
		if _, err := m.Unwrap(bad.tenant, bad.blob); !errors.Is(err, fieldcrypt.ErrCorrupt) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The service's refusals are reported with their type; nothing is mistaken for a corrupt key.
	other := m
	other.KeyID = "alias/nope"
	if _, err := other.Wrap(tenant, key); err == nil || !strings.Contains(err.Error(), "NotFoundException") || errors.Is(err, fieldcrypt.ErrCorrupt) {
		t.Fatalf("unknown key: %v", err)
	}
	other.KeyID, other.SecretKey = kmstest.KeyID, "wrong"
	if _, err := other.Unwrap(tenant, wrapped); err == nil || !strings.Contains(err.Error(), "403 IncompleteSignatureException") {
		t.Fatalf("bad credentials: %v", err)
	}
	srv.Fail = http.StatusInternalServerError
	if _, err := m.Wrap(tenant, key); err == nil || !strings.Contains(err.Error(), "kms Encrypt: 500 KMSInternalException") {
		t.Fatalf("service down: %v", err)
	}
	srv.FailType = "AccessDeniedException"
	if _, err := m.Unwrap(tenant, wrapped); err == nil || !strings.Contains(err.Error(), "kms Decrypt: 500 AccessDeniedException") {
		t.Fatalf("access denied: %v", err)
	}
	srv.FailPlain = true
	if _, err := m.Wrap(tenant, key); err == nil || !strings.Contains(err.Error(), "unexpected answer: gateway says no") {
		t.Fatalf("plain failure: %v", err)
	}
	srv.Fail, srv.FailPlain = 0, false
	// Transport failures and an endpoint that is not a URL.
	down := m
	down.Endpoint = "http://127.0.0.1:1"
	if _, err := down.Wrap(tenant, key); err == nil || !strings.Contains(err.Error(), "kms Encrypt:") {
		t.Fatalf("unreachable: %v", err)
	}
	down.Endpoint = "::"
	if _, err := down.Wrap(tenant, key); err == nil || !strings.Contains(err.Error(), "fieldcrypt: kms:") {
		t.Fatalf("bad endpoint: %v", err)
	}
	// An answer that is not JSON, with the injected clock and client.
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("<html>")) }))
	defer garbage.Close()
	var ticks int
	odd := fieldcrypt.KMS{KeyID: kmstest.KeyID, Endpoint: garbage.URL, Region: kmstest.Region, AccessKey: kmstest.AccessKey, SecretKey: kmstest.SecretKey,
		Client: garbage.Client(), Now: func() time.Time { ticks++; return time.Now() }}
	if _, err := odd.Wrap(tenant, key); err == nil || !strings.Contains(err.Error(), "answer is not JSON") || ticks != 1 {
		t.Fatalf("garbage: %v (%d)", err, ticks)
	}
	// The double itself: an unknown action and a request that is not the protocol.
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unsigned: %v %v", resp, err)
	}
	resp.Body.Close()
}

type provider struct {
	c   awscreds.Credentials
	err error
}

func (p provider) Credentials(context.Context) (awscreds.Credentials, error) { return p.c, p.err }

// TestKMSAmbientCredentials: the master signs with a credentials provider,
// session token included, and reports a provider with nothing to give.
func TestKMSAmbientCredentials(t *testing.T) {
	srv := kmstest.New(t)
	m := srv.Master("")
	m.AccessKey, m.SecretKey = "", ""
	m.Credentials = provider{c: awscreds.Credentials{AccessKey: kmstest.AccessKey, SecretKey: kmstest.SecretKey, SessionToken: "session"}}
	tenant := uuid.New()
	wrapped, err := m.Wrap(tenant, []byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := m.Unwrap(tenant, wrapped); err != nil || len(got) != 32 {
		t.Fatalf("%v %v", got, err)
	}
	m.Credentials = provider{err: awscreds.ErrNone}
	if _, err := m.Wrap(tenant, []byte("k")); err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("without credentials: %v", err)
	}
}
