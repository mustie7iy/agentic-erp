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
const Prefix = "analytics/v1"

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

// ManifestKey is the key of the manifest a run with cutoff writes.
func ManifestKey(tenant uuid.UUID, cutoff uint64) string {
	return tenantPrefix(tenant) + "/manifest-" + seq(cutoff) + ".json"
}

// write stores the new lines' files and the manifest that adds them to the
// previous manifest's, and returns the manifest's key and as_of.
func (x Extractor) write(ctx context.Context, tenant uuid.UUID, s snapshot) (string, time.Time, error) {
	m := Manifest{TenantID: tenant, Datasets: map[string]Dataset{}}
	if s.prev.set {
		b, _, err := x.Store.Get(ctx, s.prev.key)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("previous manifest: %w", err)
		}
		if err := json.Unmarshal(b, &m); err != nil {
			return "", time.Time{}, fmt.Errorf("previous manifest %s: %w", s.prev.key, err)
		}
	}
	ds := m.Datasets[glLinesDataset]
	ds.Version, ds.Watermark, ds.AsOf = 1, s.cutoff, s.asOf
	months, parts := byMonth(s.lines)
	for _, month := range months {
		f, err := x.part(ctx, tenant, month, s, parts[month])
		if err != nil {
			return "", time.Time{}, err
		}
		ds.Files, ds.Rows = append(ds.Files, f), ds.Rows+f.Rows
	}
	m.Datasets[glLinesDataset] = ds
	key := ManifestKey(tenant, s.cutoff)
	b, _ := json.Marshal(m) // a Manifest holds strings, numbers and times only, which always marshal
	asOf, err := x.putManifest(ctx, key, b, m)
	return key, asOf, err
}

// part stores one month's lines.
// A part is named by the interval it holds (from, cutoff), so a run on a
// lagging replica, which read from an older watermark, never meets another
// run's file of the same cutoff.
func (x Extractor) part(ctx context.Context, tenant uuid.UUID, month string, s snapshot, lines []glLine) (File, error) {
	b := encode(lines)
	key := tenantPrefix(tenant) + "/" + glLinesDataset + "/month=" + month + "/part-" + seq(s.prev.watermark) + "-" + seq(s.cutoff) + ".parquet"
	if err := x.putOnce(ctx, key, b, "application/vnd.apache.parquet"); err != nil {
		return File{}, err
	}
	sum := sha256.Sum256(b)
	return File{Key: key, Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:]), Rows: int64(len(lines))}, nil
}

// putOnce writes a file once: a run repeated with the same cutoff (its
// publication lost) finds the same bytes there and goes on; other bytes
// under the key are an error, never overwritten.
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

// putManifest writes the manifest once. A repeated run's manifest differs
// from the stored one only in as_of (the transaction's time), so a stored
// manifest listing the same files is taken as this run's, with its as_of,
// which the pointer then carries too.
func (x Extractor) putManifest(ctx context.Context, key string, b []byte, m Manifest) (time.Time, error) {
	asOf := m.Datasets[glLinesDataset].AsOf
	err := x.Store.Put(ctx, key, b, blobstore.PutOptions{ContentType: "application/json"})
	if !errors.Is(err, blobstore.ErrExists) {
		return asOf, err
	}
	got, _, err := x.Store.Get(ctx, key)
	if err != nil {
		return asOf, err
	}
	var stored Manifest
	if err := json.Unmarshal(got, &stored); err != nil {
		return asOf, fmt.Errorf("manifest %s: %w", key, err)
	}
	if !reflect.DeepEqual(withoutAsOf(stored), withoutAsOf(m)) {
		return asOf, fmt.Errorf("%s holds another manifest", key)
	}
	return stored.Datasets[glLinesDataset].AsOf, nil
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
