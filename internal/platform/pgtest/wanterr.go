package pgtest

import (
	"strings"
	"testing"
)

// WantErr fails the test now unless err is an error whose message contains
// want. It replaces the hand-written `err == nil || !strings.Contains(...)`
// check, whose failure message each test used to phrase its own way. It
// lives here because every package's tests already import pgtest.
func WantErr(tb testing.TB, err error, want string) {
	tb.Helper()
	if !hasErr(err, want) {
		tb.Fatalf("got %v, want an error containing %q", err, want)
	}
}

// CheckErr is WantErr for a check the rest of the test does not depend on:
// it records the failure and lets the test go on, and reports whether the
// check passed.
func CheckErr(tb testing.TB, err error, want string) bool {
	tb.Helper()
	if !hasErr(err, want) {
		tb.Errorf("got %v, want an error containing %q", err, want)
		return false
	}
	return true
}

func hasErr(err error, want string) bool {
	return err != nil && strings.Contains(err.Error(), want)
}
