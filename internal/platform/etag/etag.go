// Package etag maps resource versions to entity tags and checks If-Match
// (ADR-0008): a version mismatch is 412 and nothing changes.
package etag

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Format is the strong entity tag of a version.
func Format(version int32) string { return `"` + strconv.Itoa(int(version)) + `"` }

// Of is the strong entity tag of a resource without a version (effective-
// dated grants, insert-only documents): derived from its representation, so
// it changes whenever the representation does.
func Of(representation any) string {
	b, _ := json.Marshal(representation) // API representations always encode
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:12]) + `"`
}

// Check compares an If-Match header with the current version. "*" matches
// any version; a list matches if any entry does.
func Check(ifMatch string, version int32) error {
	return Match(ifMatch, Format(version))
}

// Match compares an If-Match header with the current entity tag.
func Match(ifMatch, want string) error {
	if strings.TrimSpace(ifMatch) == "" {
		return problem.PreconditionRequired("If-Match")
	}
	for _, tag := range strings.Split(ifMatch, ",") {
		tag = strings.TrimSpace(tag)
		if tag == "*" || tag == want {
			return nil
		}
	}
	return problem.PreconditionFailed("The resource has changed (current ETag " + want + "); read it again and retry.")
}
