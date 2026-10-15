package fieldcrypt

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestSealOpenAndFingerprint(t *testing.T) {
	if _, err := New([]byte("short")); err == nil {
		t.Fatal("short secret accepted")
	}
	k, err := New([]byte(strings.Repeat("s", 32)))
	if err != nil {
		t.Fatal(err)
	}
	tenant, other := uuid.New(), uuid.New()
	a, b := k.Seal(tenant, "de89 3704 0044 0532 0130 00"), k.Seal(tenant, "DE89370400440532013000")
	if bytes.Equal(a, b) {
		t.Fatal("sealing is deterministic")
	}
	if v, err := k.Open(tenant, a); err != nil || v != "DE89370400440532013000" {
		t.Fatalf("open: %q %v", v, err)
	}
	if _, err := k.Open(other, a); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("another tenant opened the value: %v", err)
	}
	if _, err := k.Open(tenant, a[:5]); !errors.Is(err, ErrCorrupt) {
		t.Fatal("truncated value opened")
	}
	bad := append([]byte{9}, a[1:]...)
	if _, err := k.Open(tenant, bad); !errors.Is(err, ErrCorrupt) {
		t.Fatal("unknown version opened")
	}
	if k.Fingerprint(tenant, "de89 3704") != k.Fingerprint(tenant, "DE893704") {
		t.Fatal("fingerprints differ for the same normalized value")
	}
	if k.Fingerprint(tenant, "DE893704") == k.Fingerprint(other, "DE893704") {
		t.Fatal("fingerprints are equal across tenants")
	}
	if len(k.Fingerprint(tenant, "x")) != 64 {
		t.Fatal("fingerprint is not 64 hex characters")
	}
	if got := Last4("de89 3704 0044 0532 0130 00"); got != "3000" {
		t.Errorf("Last4 of an IBAN: got %q, want %q", got, "3000")
	}
	if got := Last4("ab1"); got != "AB1" {
		t.Errorf("Last4 of a short value: got %q, want %q", got, "AB1")
	}
}
