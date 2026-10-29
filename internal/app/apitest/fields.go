package apitest

import (
	"context"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// Object, List and String read one field of a decoded JSON body and fail
// the test when it is missing or of another type. A test that goes on after
// a failed check (t.Errorf) and then reads a nested field with a bare type
// assertion would panic instead, and the panic stops every other test in the
// package; these keep that a reported failure.

// Object is the field key of m as a JSON object.
func Object(tb testing.TB, m map[string]any, key string) map[string]any {
	tb.Helper()
	v, ok := m[key].(map[string]any)
	if !ok {
		tb.Fatalf("%s: got %v, want an object", key, m[key])
	}
	return v
}

// List is the field key of m as a JSON array.
func List(tb testing.TB, m map[string]any, key string) []any {
	tb.Helper()
	v, ok := m[key].([]any)
	if !ok {
		tb.Fatalf("%s: got %v, want an array", key, m[key])
	}
	return v
}

// String is the field key of m as a JSON string.
func String(tb testing.TB, m map[string]any, key string) string {
	tb.Helper()
	v, ok := m[key].(string)
	if !ok {
		tb.Fatalf("%s: got %v, want a string", key, m[key])
	}
	return v
}

// HouseBankCode reads a house bank's code, for WantNamed.
const HouseBankCode = `SELECT code FROM payments.house_bank WHERE id = $1`

// WantNamed checks that obj, a decoded resource (what names it in a
// failure), gives nameField exactly when it gives idField, and as query
// reads it for the id ($1) from the owning table: a response names what it
// references by id (ADR-0038 As built).
func WantNamed(tb testing.TB, what string, obj map[string]any, idField, nameField, query string) {
	tb.Helper()
	ref, ok := obj[idField]
	if !ok {
		if name, has := obj[nameField]; has {
			tb.Errorf("%s %v without %s: got %s %v, want none", what, obj["id"], idField, nameField, name)
		}
		return
	}
	var want string
	if err := pgtest.Admin(tb).QueryRow(context.Background(), query, ref).Scan(&want); err != nil {
		tb.Fatalf("%s %v: %s %v: %v", what, obj["id"], idField, ref, err)
	}
	if obj[nameField] != want {
		tb.Errorf("%s %v: got %s %v, want %s", what, obj["id"], nameField, obj[nameField], want)
	}
}
