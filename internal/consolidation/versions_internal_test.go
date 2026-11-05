package consolidation

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
)

// TestVersionCache: a version is read once while kept, per tenant; a
// failed read keeps nothing; beyond maxVersions the least recently used
// is read again.
func TestVersionCache(t *testing.T) {
	var c versionCache
	reads := map[versionKey]int{}
	get := func(k versionKey) {
		t.Helper()
		if _, err := c.get(k, func() (*config.Config, error) { reads[k]++; return &config.Config{}, nil }); err != nil {
			t.Fatal(err)
		}
	}
	a, b := uuid.New(), uuid.New()
	if _, err := c.get(versionKey{a, 9}, func() (*config.Config, error) { return nil, errors.New("down") }); err == nil {
		t.Error("a failed read: got no error, want it")
	}
	for _, n := range []int32{1, 2, 3, 1} {
		get(versionKey{a, n})
	}
	get(versionKey{b, 1}) // another tenant's version 1 is its own
	get(versionKey{a, 4}) // drops a's version 2, the least recently used
	get(versionKey{a, 1})
	get(versionKey{a, 2})
	want := map[versionKey]int{{a, 1}: 1, {a, 2}: 2, {a, 3}: 1, {a, 4}: 1, {b, 1}: 1}
	for k, n := range want {
		if reads[k] != n {
			t.Errorf("reads of %v: got %d, want %d", k.number, reads[k], n)
		}
	}
	if _, kept := c.docs[versionKey{a, 9}]; kept {
		t.Error("a failed read: got it kept, want nothing")
	}
}

// TestVersionOutsideRequest: a version is read only in a unit of work.
func TestVersionOutsideRequest(t *testing.T) {
	if _, err := (&Endpoints{}).version(context.Background(), 1); err == nil {
		t.Error("outside a unit of work: got no error, want one")
	}
}
