// Package buildinfo is what a binary knows of its own build (ADR-0032): the
// release version stamped at link time and the source revision Go records in
// every binary built from a git checkout. The servers log it at start and
// serve it on their admin listener, so an operator can tell which release
// and commit a running image is without trusting its tag.
package buildinfo

import (
	"encoding/json"
	"net/http"
	"runtime/debug"
)

// version is the release version (v1.2.3), set at link time by the image
// build: -ldflags "-X github.com/mustie7iy/agentic-erp/internal/platform/buildinfo.version=v1.2.3".
// A build without it reports Development.
var version string

// Development is the version of a build that was not stamped with one.
const Development = "devel"

// Info describes a build.
type Info struct {
	Version  string `json:"version"`
	Revision string `json:"revision,omitempty"` // git commit
	Time     string `json:"time,omitempty"`     // commit time, RFC 3339
	Modified bool   `json:"modified"`           // built from a checkout with uncommitted changes
}

// Get is this binary's build.
func Get() Info { return from(version, debug.ReadBuildInfo) }

// from builds an Info from a stamped version and Go's build information,
// which a test binary or a build outside a checkout may lack.
func from(stamped string, read func() (*debug.BuildInfo, bool)) Info {
	i := Info{Version: stamped}
	if i.Version == "" {
		i.Version = Development
	}
	bi, ok := read()
	if !ok {
		return i
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			i.Revision = s.Value
		case "vcs.time":
			i.Time = s.Value
		case "vcs.modified":
			i.Modified = s.Value == "true"
		}
	}
	return i
}

// Handler serves the build as JSON (GET /version on the admin listeners).
func Handler() http.Handler {
	body, _ := json.Marshal(Get()) // strings and a bool always encode
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body) // a failed write means the client is gone: no one is left to tell
	})
}
