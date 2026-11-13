package extract

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/parquet-go/parquet-go"
	"gopkg.in/yaml.v3"

	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore/blobstoretest"
)

// TestColumnsAreTheContract: each dataset's Parquet columns are exactly its
// contract file's, in its order (ADR-0044: datasets are a contract).
func TestColumnsAreTheContract(t *testing.T) {
	for name, row := range map[string]any{
		"gl_lines": glLine{}, "po_lines": poLine{}, "ap_invoices": apInvoice{},
		"open_items": openItem{}, "accounts": accountRow{}, "partners": partnerRow{}, "sales_lines": salesLine{}, "spend_lines": spendLine{},
	} {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile("../../../analytics/datasets/v1/" + name + ".yaml")
			if err != nil {
				t.Fatal(err)
			}
			var contract struct {
				Columns []struct {
					Name     string `yaml:"name"`
					Nullable bool   `yaml:"nullable"`
				} `yaml:"columns"`
			}
			if err := yaml.Unmarshal(b, &contract); err != nil {
				t.Fatal(err)
			}
			var want, got []string
			for _, c := range contract.Columns {
				want = append(want, c.Name)
			}
			for _, f := range parquet.SchemaOf(row).Fields() {
				got = append(got, f.Name())
				i := slices.Index(want, f.Name())
				if i >= 0 && f.Optional() != contract.Columns[i].Nullable {
					t.Errorf("column %s: optional %v, want %v", f.Name(), f.Optional(), contract.Columns[i].Nullable)
				}
			}
			if !slices.Equal(got, want) {
				t.Errorf("columns: got %v, want %v", got, want)
			}
		})
	}
}

func TestXIDRefusesText(t *testing.T) {
	var x xid
	if err := x.ScanText(pgtype.Text{String: "x", Valid: true}); err == nil {
		t.Error("scan of x: got nil error, want an error")
	}
}

// getFails is a store whose Get fails.
type getFails struct{ blobstore.Store }

func (getFails) Get(context.Context, string) ([]byte, string, error) {
	return nil, "", blobstore.ErrNotFound
}

// TestWritesOnce: a key written before holds the same content or the run
// stops; it is never overwritten.
func TestWritesOnce(t *testing.T) {
	ctx := context.Background()
	store := blobstoretest.New(t).Store()
	x := Extractor{Store: store}
	stored := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	tenant := uuid.New()
	// runAt is a run with cutoff whose manifest lists one file, as of asOf.
	runAt := func(cutoff uint64, asOf time.Time) *run {
		r := &run{tenant: tenant, cutoff: cutoff, asOf: asOf, m: Manifest{TenantID: tenant, Datasets: map[string]Dataset{}}}
		r.m.add(glLinesDataset, Dataset{Rows: 1, Files: []File{{Key: "k"}}}, r)
		return r
	}
	// plant stores content under the key of the manifest of run r.
	plant := func(r *run, content []byte) {
		if err := store.Put(ctx, ManifestKey(tenant, r.cutoff, digestOf(r.m)), content, blobstore.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Put(ctx, "file", []byte("abc"), blobstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	first := runAt(1, stored)
	b, _ := json.Marshal(first.m) // a Manifest always marshals
	plant(first, b)
	t.Run("same manifest", func(t *testing.T) {
		// A repeated run's manifest is the stored one but for its later
		// as_of, and the run takes the stored as_of.
		again := runAt(1, stored.Add(time.Minute))
		if _, err := x.putManifest(ctx, again); err != nil || !again.m.asOf().Equal(stored) {
			t.Errorf("got as_of %v, %v, want the stored as_of %v", again.m.asOf(), err, stored)
		}
	})
	t.Run("same file", func(t *testing.T) {
		if err := x.putOnce(ctx, "file", []byte("abc"), ""); err != nil {
			t.Errorf("got %v, want nil", err)
		}
	})
	other, broken := runAt(2, stored), runAt(3, stored)
	plant(other, []byte(`{"tenant_id": "`+uuid.NewString()+`"}`))
	plant(broken, []byte("{"))
	errOf := func(_ string, err error) error { return err }
	for name, err := range map[string]error{
		"other file":             x.putOnce(ctx, "file", []byte("abd"), ""),
		"file unreadable":        Extractor{Store: getFails{store}}.putOnce(ctx, "file", []byte("abc"), ""),
		"other manifest":         errOf(x.putManifest(ctx, other)),
		"manifest unreadable":    errOf(Extractor{Store: getFails{store}}.putManifest(ctx, runAt(1, stored))),
		"stored manifest broken": errOf(x.putManifest(ctx, broken)),
	} {
		t.Run(name, func(t *testing.T) {
			if err == nil {
				t.Error("got nil error, want an error")
			}
		})
	}
}
