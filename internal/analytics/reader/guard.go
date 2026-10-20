package reader

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/analytics/extract"
)

// ErrRefused means the reader would not read what a request or a manifest
// names: a key outside the tenant's prefix, a file whose content is not the
// manifest's, a plan outside the contract or the principal's scope. The
// semantic layer answers from the database instead (ADR-0044).
var ErrRefused = errors.New("reader: refused")

func refused(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

// The layout the extractor writes (ADR-0044) and nothing else: a manifest
// named by its cutoff and digest, and a dataset's part named by the
// journal interval it holds or the SHA-256 of its content, under its month
// for a fact dataset. Anchored, so no "..", glob, scheme or second tenant
// can appear around them.
var (
	manifestName = regexp.MustCompile(`^manifest-[0-9]{20}(-[0-9a-f]{16})?\.json$`)
	partName     = regexp.MustCompile(`^(month=[0-9]{4}-(0[1-9]|1[0-2])/)?part-([0-9]{20}-[0-9]{20}|[0-9a-f]{64})\.parquet$`)
	datasetName  = regexp.MustCompile(`^[a-z][a-z_]{0,62}$`)
	sha256Hex    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// tenantPrefix is the directory a tenant's keys must be in, with its slash.
func tenantPrefix(tenant uuid.UUID) string {
	return extract.Prefix + "/tenant=" + tenant.String() + "/"
}

// checkManifestKey accepts only a manifest of tenant.
func checkManifestKey(tenant uuid.UUID, key string) error {
	rest, ok := strings.CutPrefix(key, tenantPrefix(tenant))
	if !ok || !manifestName.MatchString(rest) {
		return refused("manifest %q is not one of tenant %s", key, tenant)
	}
	return nil
}

// checkFileKey accepts only a part of dataset under tenant's prefix, and a
// digest the reader can check the content against.
func checkFileKey(tenant uuid.UUID, dataset string, f extract.File) error {
	if !datasetName.MatchString(dataset) {
		return refused("dataset name %q", dataset)
	}
	rest, ok := strings.CutPrefix(f.Key, tenantPrefix(tenant)+dataset+"/")
	if !ok || !partName.MatchString(rest) {
		return refused("file %q is not a part of %s of tenant %s", f.Key, dataset, tenant)
	}
	if !sha256Hex.MatchString(f.SHA256) || f.Size < 0 {
		return refused("file %q: digest %q, size %d", f.Key, f.SHA256, f.Size)
	}
	return nil
}
