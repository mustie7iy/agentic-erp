package sap

import "testing"

func FuzzODataValue(f *testing.F) {
	for _, s := range []string{"/Date(1767225600000)/", "/Date(-1)/", "/Date(abc)/", "/Date(1+0100)/", "plain"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) { odataValue(s) })
}
