package api

import (
	"testing"
	"time"
)

// op is a one-operation contract with extra lines under the operation.
func op(extra string) []byte {
	return []byte("paths:\n  /v1/x:\n    get:\n      operationId: getX\n      x-permission: a.b.c\n" + extra)
}

func TestLifecycle(t *testing.T) {
	ops, err := parseOperations(op("      deprecated: true\n      x-deprecated-at: '2027-01-01'\n      x-sunset: '2028-01-01'\n"+
		"      x-stability-level: beta\n      externalDocs: {url: 'https://docs.example.com/v1/x'}\n"), true)
	if err != nil {
		t.Fatalf("deprecated operation: %v", err)
	}
	got := ops["getX"]
	want := Lifecycle{Deprecated: true, DeprecatedAt: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		Sunset: time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC), Link: "https://docs.example.com/v1/x"}
	if got.Lifecycle != want {
		t.Errorf("lifecycle: got %+v, want %+v", got.Lifecycle, want)
	}
	if got.Stability != "beta" {
		t.Errorf("stability: got %q, want beta", got.Stability)
	}
	ops, err = parseOperations(op(""), true)
	if err != nil {
		t.Fatalf("plain operation: %v", err)
	}
	if ops["getX"].Lifecycle.Deprecated || ops["getX"].Stability != "stable" {
		t.Errorf("plain operation: got %+v, want not deprecated and stable", ops["getX"])
	}
}

func TestLifecycleRejects(t *testing.T) {
	const docs = "      externalDocs: {url: 'https://docs.example.com/v1/x'}\n"
	for name, extra := range map[string]string{
		"unknown stability":   "      x-stability-level: experimental\n",
		"sunset without flag": "      x-sunset: '2028-01-01'\n",
		"date without flag":   "      x-deprecated-at: '2027-01-01'\n",
		"no deprecation date": "      deprecated: true\n      x-sunset: '2028-01-01'\n" + docs,
		"bad sunset":          "      deprecated: true\n      x-deprecated-at: '2027-01-01'\n      x-sunset: soon\n" + docs,
		"short notice":        "      deprecated: true\n      x-deprecated-at: '2027-01-01'\n      x-sunset: '2027-12-31'\n" + docs,
		"no migration notes":  "      deprecated: true\n      x-deprecated-at: '2027-01-01'\n      x-sunset: '2028-01-01'\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseOperations(op(extra), true); err == nil {
				t.Error("got no error, want the operation refused")
			}
		})
	}
}
