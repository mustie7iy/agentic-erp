package inventory

import "testing"

// SetMaxABCLines lowers the most materials an ABC analysis lists for the
// test, restoring it after.
func SetMaxABCLines(tb testing.TB, n int) {
	tb.Helper()
	old := maxABCLines
	maxABCLines = n
	tb.Cleanup(func() { maxABCLines = old })
}
