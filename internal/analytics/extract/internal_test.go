package extract

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/parquet-go/parquet-go"
	"gopkg.in/yaml.v3"

	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore/blobstoretest"
)

// TestColumnsAreTheContract: the Parquet columns are exactly the dataset
// file's, in its order (ADR-0044: datasets are a contract).
func TestColumnsAreTheContract(t *testing.T) {
	b, err := os.ReadFile("../../../analytics/datasets/v1/gl_lines.yaml")
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
	for _, f := range parquet.SchemaOf(glLine{}).Fields() {
		got = append(got, f.Name())
		i := slices.Index(want, f.Name())
		if i >= 0 && f.Optional() != contract.Columns[i].Nullable {
			t.Errorf("column %s: optional %v, want %v", f.Name(), f.Optional(), contract.Columns[i].Nullable)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("columns: got %v, want %v", got, want)
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
	m := Manifest{TenantID: uuid.New(), Datasets: map[string]Dataset{"gl_lines": {Version: 1, Rows: 1, Files: []File{{Key: "k"}}}}}
	b, _ := json.Marshal(m) // a Manifest always marshals
	for key, content := range map[string]string{"file": "abc", "manifest": string(b), "garbage": "{"} {
		if err := store.Put(ctx, key, []byte(content), blobstore.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	other := m
	other.TenantID = uuid.New()
	for name, err := range map[string]error{
		"same file":     x.putOnce(ctx, "file", []byte("abc"), ""),
		"same manifest": x.putManifest(ctx, "manifest", b, m),
	} {
		t.Run(name, func(t *testing.T) {
			if err != nil {
				t.Errorf("got %v, want nil", err)
			}
		})
	}
	for name, err := range map[string]error{
		"other file":             x.putOnce(ctx, "file", []byte("abd"), ""),
		"file unreadable":        Extractor{Store: getFails{store}}.putOnce(ctx, "file", []byte("abc"), ""),
		"other manifest":         x.putManifest(ctx, "manifest", b, other),
		"manifest unreadable":    Extractor{Store: getFails{store}}.putManifest(ctx, "manifest", b, m),
		"stored manifest broken": x.putManifest(ctx, "garbage", b, m),
	} {
		t.Run(name, func(t *testing.T) {
			if err == nil {
				t.Error("got nil error, want an error")
			}
		})
	}
}
