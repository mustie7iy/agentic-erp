package payments_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// failingStore refuses every write and read.
type failingStore struct{ err error }

func (f failingStore) Put(context.Context, string, []byte, blobstore.PutOptions) error { return f.err }
func (f failingStore) Get(context.Context, string) ([]byte, string, error)             { return nil, "", f.err }
func (f failingStore) Stat(context.Context, string) (int64, string, error)             { return 0, "", f.err }

// TestBankFilesAreKept: with object storage the file written at release is
// kept and handed out or transmitted from there, always checked against the
// recorded hash.
func TestBankFilesAreKept(t *testing.T) {
	e := setup(t)
	root := t.TempDir()
	store := blobstore.Dir{Root: root}
	e.API = apitest.NewWith(t, func(cfg *httpapi.Config) { cfg.Connector, cfg.Objects = e.connector, store })
	e.objects = store
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
	run := e.run(t, "2026-04-15", "2026-04-15")
	e.release(t, run, "")
	files, _ := e.Must(200, "GET", "/v1/bank-files?payment_run_id="+id(run), "", "")
	file := files["items"].([]any)[0].(map[string]any)
	first := uuidOf(id(file))
	key := blobstore.Key(e.Tenant.ID, blobstore.KindBankFiles, id(file)+".xml")
	stored, ct, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("read the stored file: %v", err)
	}
	if ct != "application/xml" {
		t.Errorf("stored file content type: got %s, want application/xml", ct)
	}
	if want := "<MsgId>2000-" + run["document_number"].(string) + "</MsgId>"; !strings.Contains(string(stored), want) {
		t.Errorf("stored file: got\n%s\nwant it to contain %s", stored, want)
	}
	var ref string
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT object_ref FROM payments.bank_file WHERE id = $1`, id(file)).Scan(&ref); err != nil {
		t.Fatalf("read object_ref: %v", err)
	}
	if ref != key {
		t.Errorf("object_ref: got %q, want %q", ref, key)
	}
	r := e.API.Do(apitest.Req{Method: "GET", Path: "/v1/bank-files/" + id(file) + "/content", Tenant: e.Tenant.ID, As: e.Tenant.Human})
	if r.Code != 200 {
		t.Fatalf("content status: got %d, want 200: %s", r.Code, r.Body)
	}
	if r.Body.String() != string(stored) {
		t.Errorf("content: got %s, want the stored file", r.Body)
	}
	if got := r.Header().Get("Content-Type"); got != "application/xml" {
		t.Errorf("content type: got %s, want application/xml", got)
	}
	// Bank details are inside: agents never read it; the file is transmitted from the store.
	e.As(e.Tenant.Agent).Problem(403, "identity.agent_not_allowed", "GET", "/v1/bank-files/"+id(file)+"/content", "", "")
	e.Must(202, "POST", "/v1/bank-files/"+id(file)+":transmit", "", `"1"`)
	if len(e.connector.files) != 1 || string(e.connector.files[0].Content) != string(stored) {
		t.Fatalf("transmitted: %v", e.connector.files)
	}
	e.Problem(404, "request.not_found", "GET", "/v1/bank-files/01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d01/content", "", "")

	// A second run: the stored object is altered, so neither download nor transmission hands it out.
	e.invoice(t, e.Supplier, "INV-2", "50.00", "2026-03-02")
	second := e.run(t, "2026-04-16", "2026-04-16")
	e.release(t, second, "")
	files, _ = e.Must(200, "GET", "/v1/bank-files?payment_run_id="+id(second), "", "")
	file = files["items"].([]any)[0].(map[string]any)
	path := filepath.Join(root, filepath.FromSlash(blobstore.Key(e.Tenant.ID, blobstore.KindBankFiles, id(file)+".xml")))
	if err := os.WriteFile(path, []byte("<Document>tampered</Document>"), 0o600); err != nil {
		t.Fatalf("tamper with the bank file: %v", err)
	}
	e.Problem(409, "payments.bank_file_changed", "GET", "/v1/bank-files/"+id(file)+"/content", "", "")
	e.Problem(409, "payments.bank_file_changed", "POST", "/v1/bank-files/"+id(file)+":transmit", "", `"1"`)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the bank file: %v", err)
	}
	e.Problem(503, "platform.unavailable", "GET", "/v1/bank-files/"+id(file)+"/content", "", "")
	// A server without object storage cannot serve a stored file; one whose store fails neither.
	plain := apitest.NewWith(t, func(cfg *httpapi.Config) { cfg.Connector = e.connector })
	if r := plain.Do(apitest.Req{Method: "GET", Path: "/v1/bank-files/" + id(file) + "/content", Tenant: e.Tenant.ID, As: e.Tenant.Human}); r.Code != 503 {
		t.Fatalf("without a store: %d %s", r.Code, r.Body)
	}
	broken := failingStore{err: errors.New("store down")}
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		h := endpoints(e.connector)
		h.Objects = broken
		_, err := h.FileContent(ctx, uuidOf(id(file)))
		return err
	}); problem.From(err).Status != 503 {
		t.Fatalf("broken store: %v", err)
	}
	// A release whose file the store refuses does not go through.
	e.API = apitest.NewWith(t, func(cfg *httpapi.Config) { cfg.Connector, cfg.Objects = e.connector, broken })
	e.objects = broken
	e.invoice(t, e.Supplier, "INV-3", "25.00", "2026-03-03")
	third := e.run(t, "2026-04-17", "2026-04-17")
	// The store's failure is the machinery's, not a business problem: the worker reports it and the run stays releasing, its payment posted, for the next attempt.
	op := e.decide(t, 202, third, "release", "")
	if _, err := e.releaser(t).RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "store") {
		t.Fatalf("release with a broken store: %v", err)
	}
	if got, _ := e.Must(200, "GET", "/v1/payment-runs/"+id(third), "", ""); got["status"] != "releasing" {
		t.Fatalf("run with a broken store: %v", got)
	}
	if got, _ := e.Must(200, "GET", "/v1/operations/"+id(op), "", ""); got["status"] != "running" {
		t.Fatalf("operation with a broken store: %v", got)
	}
	e.Problem(409, "payments.release_in_progress", "POST", "/v1/payment-runs/"+id(third)+":release", "", "*")
	// With the store back, the worker finishes it.
	e.objects = store
	if _, err := e.releaser(t).RunOnce(context.Background()); err != nil {
		t.Fatalf("resumed release: %v", err)
	}
	if got, _ := e.Must(200, "GET", "/v1/payment-runs/"+id(third), "", ""); got["status"] != "released" {
		t.Fatalf("resumed run: %v", got)
	}
	if _, err := endpoints(nil).FileContent(context.Background(), uuidOf(id(file))); err == nil {
		t.Error("content outside a unit of work")
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "content", func(ctx context.Context) error {
		h := endpoints(e.connector)
		h.Objects = store
		_, err := h.FileContent(ctx, first)
		return err
	})
	_ = payments.FileCreated
}
