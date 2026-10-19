package offboarding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
)

// ErrNoExport means the object store holds no export of the tenant.
var ErrNoExport = errors.New("offboarding: no export of the tenant in the object store")

// Adopt records the tenant's newest export already in the object store as
// its export, due for erasure at once (ADR-0040, T41f). A cell restored to
// a point before it exported and erased a tenant lacks the record of both;
// the export, locked, is still there, so the tenant is erased again
// against it rather than exported a second time (a new locked copy of
// data that was erased). ErrNoExport when there is none; the store must
// list (blobstore.Eraser).
func (w *Worker) Adopt(ctx context.Context, tenant uuid.UUID) (Export, error) {
	lister, ok := w.Store.(blobstore.Eraser)
	if !ok {
		return Export{}, fmt.Errorf("offboarding: adopt tenant %s: the object store cannot list", tenant)
	}
	keys, err := lister.List(ctx, blobstore.Key(tenant, blobstore.KindExports, ""))
	if err != nil {
		return Export{}, fmt.Errorf("offboarding: adopt tenant %s: %w", tenant, err)
	}
	// An export's directory is named by its offboarding's Unix time, ten
	// digits until 2286: the newest sorts last.
	manifests := slices.DeleteFunc(keys, func(k string) bool { return !strings.HasSuffix(k, "/manifest.json") })
	if len(manifests) == 0 {
		return Export{}, fmt.Errorf("%w: tenant %s", ErrNoExport, tenant)
	}
	ref := slices.Max(manifests)
	raw, _, err := w.Store.Get(ctx, ref)
	if err != nil {
		return Export{}, fmt.Errorf("offboarding: adopt %s: %w", ref, err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil || m.TenantID != tenant {
		return Export{}, fmt.Errorf("offboarding: adopt %s: not a manifest of tenant %s", ref, tenant)
	}
	now := w.now()
	ex := Export{Ref: ref, Tables: len(m.Tables), ExportedAt: now, EraseAfter: now}
	for _, t := range m.Tables {
		ex.Rows += t.Rows
	}
	if err := w.recordExport(ctx, tenant, ex); err != nil {
		return Export{}, err
	}
	return ex, nil
}
