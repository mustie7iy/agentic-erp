package buildinfo

import (
	"encoding/json"
	"net/http/httptest"
	"runtime/debug"
	"testing"
)

func TestFrom(t *testing.T) {
	recorded := &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "-trimpath", Value: "true"},
		{Key: "vcs.revision", Value: "0123abcd"},
		{Key: "vcs.time", Value: "2026-10-16T12:00:00Z"},
		{Key: "vcs.modified", Value: "true"},
	}}
	for name, c := range map[string]struct {
		stamped string
		info    *debug.BuildInfo
		want    Info
	}{
		"release build":        {"v1.2.3", recorded, Info{Version: "v1.2.3", Revision: "0123abcd", Time: "2026-10-16T12:00:00Z", Modified: true}},
		"development build":    {"", recorded, Info{Version: Development, Revision: "0123abcd", Time: "2026-10-16T12:00:00Z", Modified: true}},
		"no build information": {"v1.2.3", nil, Info{Version: "v1.2.3"}},
		"outside a checkout":   {"", &debug.BuildInfo{}, Info{Version: Development}},
	} {
		t.Run(name, func(t *testing.T) {
			read := func() (*debug.BuildInfo, bool) { return c.info, c.info != nil }
			if got := from(c.stamped, read); got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/version", nil))
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content type: got %q, want application/json", got)
	}
	var got Info
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body %q: %v", rec.Body, err)
	}
	if got != Get() {
		t.Errorf("body: got %+v, want %+v", got, Get())
	}
	if got.Version != Development {
		t.Errorf("version of a test binary: got %q, want %q", got.Version, Development)
	}
}
