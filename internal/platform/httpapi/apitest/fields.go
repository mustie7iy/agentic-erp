package apitest

import "testing"

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
