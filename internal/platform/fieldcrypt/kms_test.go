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
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestKMSMaster(t *testing.T) {
	srv := kmstest.New(t)
	m := srv.Master("")
	if got := m.ID(); got != "kms" {
		t.Errorf("default master id: got %q, want kms", got)
	}
	if got := srv.Master("hsm1").ID(); got != "hsm1" {
		t.Errorf("named master id: got %q, want hsm1", got)
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
	e, d := srv.Calls()
	if e != 1 {
		t.Errorf("encrypt calls: got %d, want 1", e)
	}
	if d != 1 {
		t.Errorf("decrypt calls: got %d, want 1", d)
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
		t.Run(name, func(t *testing.T) {
			_, err := m.Unwrap(bad.tenant, bad.blob)
			if !errors.Is(err, fieldcrypt.ErrCorrupt) {
				t.Errorf("got %v, want ErrCorrupt", err)
			}
			// The service's refusal stays in the chain behind ErrCorrupt.
			var both interface{ Unwrap() []error }
			if !errors.As(err, &both) || len(both.Unwrap()) != 2 {
				t.Errorf("got %v, want ErrCorrupt wrapping the service's refusal", err)
			}
		})
	}
	// The service's refusals are reported with their type; nothing is mistaken for a corrupt key.
	other := m
	other.KeyID = "alias/nope"
	_, err = other.Wrap(tenant, key)
	pgtest.CheckErr(t, err, "NotFoundException")
	if errors.Is(err, fieldcrypt.ErrCorrupt) {
		t.Errorf("unknown key: got %v, want an error that is not ErrCorrupt", err)
	}
	other.KeyID, other.SecretKey = kmstest.KeyID, "wrong"
	_, err = other.Unwrap(tenant, wrapped)
	pgtest.WantErr(t, err, "403 IncompleteSignatureException")
	srv.Fail = http.StatusInternalServerError
	_, err = m.Wrap(tenant, key)
	pgtest.WantErr(t, err, "kms Encrypt: 500 KMSInternalException")
	srv.FailType = "AccessDeniedException"
	_, err = m.Unwrap(tenant, wrapped)
	pgtest.WantErr(t, err, "kms Decrypt: 500 AccessDeniedException")
	srv.FailPlain = true
	_, err = m.Wrap(tenant, key)
	pgtest.WantErr(t, err, "unexpected answer: gateway says no")
	srv.Fail, srv.FailPlain = 0, false
	// Transport failures and an endpoint that is not a URL.
	down := m
	down.Endpoint = "http://127.0.0.1:1"
	_, err = down.Wrap(tenant, key)
	pgtest.WantErr(t, err, "kms Encrypt:")
	down.Endpoint = "::"
	_, err = down.Wrap(tenant, key)
	pgtest.WantErr(t, err, "fieldcrypt: kms:")
	// An answer that is not JSON, with the injected clock and client.
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("<html>")) }))
	defer garbage.Close()
	var ticks int
	odd := fieldcrypt.KMS{KeyID: kmstest.KeyID, Endpoint: garbage.URL, Region: kmstest.Region, AccessKey: kmstest.AccessKey, SecretKey: kmstest.SecretKey,
		Client: garbage.Client(), Now: func() time.Time { ticks++; return time.Now() }}
	_, err = odd.Wrap(tenant, key)
	pgtest.CheckErr(t, err, "answer is not JSON")
	if ticks != 1 {
		t.Errorf("injected clock reads: got %d, want 1", ticks)
	}
	// The double itself: an unknown action and a request that is not the protocol.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unsigned: %v %v", resp, err)
	}
	_ = resp.Body.Close() // the body is not read; the status code is the answer
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
	_, err = m.Wrap(tenant, []byte("k"))
	pgtest.WantErr(t, err, "credentials")
}
