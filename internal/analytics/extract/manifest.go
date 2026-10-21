package extract

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
)

// Prefix is the first segment of every key of the store: the layout's
// version, then the tenant (ADR-0044).
const Prefix = blobstore.AnalyticsPrefix

// Manifest names everything a run published for a tenant: the reader takes
// one manifest and reads only the files it lists.
type Manifest struct {
	TenantID uuid.UUID          `json:"tenant_id"`
	Datasets map[string]Dataset `json:"datasets"`
}

// Dataset is one dataset's files, the watermark they reach, their rows and
// the database state they are as of.
type Dataset struct {
	Version   int       `json:"version"`
	Watermark uint64    `json:"watermark,string"` // a string: JSON readers lose integers above 2^53
	Rows      int64     `json:"rows"`
	AsOf      time.Time `json:"as_of"`
	Files     []File    `json:"files"`
}

// File is one Parquet file of a dataset.
type File struct {
	Key    string `json:"key"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Rows   int64  `json:"rows"`
}

// tenantPrefix is where a tenant's files live.
func tenantPrefix(tenant uuid.UUID) string { return Prefix + "/tenant=" + tenant.String() }

// seq names a run's files by its cutoff, zero-padded so names sort by run;
// analytics.snapshot checks the manifest's name against its watermark.
func seq(cutoff uint64) string { return fmt.Sprintf("%020d", cutoff) }

// ManifestKey is the key of the manifest a run with cutoff writes, digest
// being the first 16 hex digits of the SHA-256 of what it lists (digestOf):
// two runs with one cutoff can see different documents (ADR-0044). An
// empty digest gives T55b's name, which the pointer still accepts.
func ManifestKey(tenant uuid.UUID, cutoff uint64, digest string) string {
	if digest != "" {
		digest = "-" + digest
	}
	return tenantPrefix(tenant) + "/manifest-" + seq(cutoff) + digest + ".json"
}

// add sets a dataset of the run's manifest: version 1, at the run's cutoff
// and as of its snapshot.
func (m Manifest) add(name string, d Dataset, r *run) {
	d.Version, d.Watermark, d.AsOf = 1, r.cutoff, r.asOf
	m.Datasets[name] = d
}

// asOf is the database state the manifest's datasets are as of; a run gives
// them all the same.
func (m Manifest) asOf() time.Time { return m.Datasets[glLinesDataset].AsOf }

// manifest reads the manifest the pointer names; before the first
// publication there is none.
func (x Extractor) manifest(ctx context.Context, tenant uuid.UUID, p pointer) (Manifest, error) {
	m := Manifest{TenantID: tenant, Datasets: map[string]Dataset{}}
	if !p.set {
		return m, nil
	}
	b, _, err := x.Store.Get(ctx, p.key)
	if err != nil {
		return m, fmt.Errorf("previous manifest: %w", err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("previous manifest %s: %w", p.key, err)
	}
	return m, nil
}

// digestOf names a manifest's content apart from its as_of, which a run
// repeated with the same files would otherwise change.
func digestOf(m Manifest) string {
	b, _ := json.Marshal(withoutAsOf(m)) // a Manifest holds strings, numbers and times only, which always marshal
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// putOnce writes a file once: a run repeated with the same interval (its
// publication lost), or a snapshot file of unchanged content, finds the
// same bytes there and goes on; other bytes under the key are an error,
// never overwritten.
func (x Extractor) putOnce(ctx context.Context, key string, b []byte, contentType string) error {
	err := x.Store.Put(ctx, key, b, blobstore.PutOptions{ContentType: contentType})
	if !errors.Is(err, blobstore.ErrExists) {
		return err
	}
	got, _, err := x.Store.Get(ctx, key)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, b) {
		return fmt.Errorf("%s holds other content", key)
	}
	return nil
}

// putManifest writes the run's manifest once and returns its key. A repeated
// run's manifest differs from the stored one only in as_of (the
// transaction's time), so a stored manifest listing the same files is taken
// as this run's, with its as_of, which the pointer then carries too.
func (x Extractor) putManifest(ctx context.Context, r *run) (string, error) {
	key := ManifestKey(r.tenant, r.cutoff, digestOf(r.m))
	b, _ := json.Marshal(r.m) // a Manifest holds strings, numbers and times only, which always marshal
	err := x.Store.Put(ctx, key, b, blobstore.PutOptions{ContentType: "application/json"})
	if !errors.Is(err, blobstore.ErrExists) {
		return key, err
	}
	got, _, err := x.Store.Get(ctx, key)
	if err != nil {
		return key, err
	}
	var stored Manifest
	if err := json.Unmarshal(got, &stored); err != nil {
		return key, fmt.Errorf("manifest %s: %w", key, err)
	}
	if !reflect.DeepEqual(withoutAsOf(stored), withoutAsOf(r.m)) {
		return key, fmt.Errorf("%s holds another manifest", key)
	}
	r.m = stored
	return key, nil
}

// withoutAsOf is m with its datasets' as_of cleared.
func withoutAsOf(m Manifest) Manifest {
	out := Manifest{TenantID: m.TenantID, Datasets: map[string]Dataset{}}
	for name, d := range m.Datasets {
		d.AsOf = time.Time{}
		out.Datasets[name] = d
	}
	return out
}
