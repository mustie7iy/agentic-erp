package payables_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// failingStore refuses every write and read.
type failingStore struct{ err error }

func (f failingStore) Put(context.Context, string, []byte, blobstore.PutOptions) error { return f.err }
func (f failingStore) Get(context.Context, string) ([]byte, string, error)             { return nil, "", f.err }
func (f failingStore) Stat(context.Context, string) (int64, string, error)             { return 0, "", f.err }

// TestCapturedDocumentsAreKept: with object storage the document is stored
// under its hash before extraction and can be downloaded, byte for byte,
// with the content type detected at capture.
func TestCapturedDocumentsAreKept(t *testing.T) {
	e := setup(t)
	e.order(t, e.Supplier, "PO-1")
	root := t.TempDir()
	store := blobstore.Dir{Root: root}
	e.API = apitest.NewWith(t, func(cfg *app.Config) { cfg.Objects = store })
	with := func(x capture.Extractor, objects blobstore.Store) *payables.Endpoints {
		h := endpoints(x)
		h.Objects = objects
		return h
	}
	capture := func(h *payables.Endpoints, doc string) (map[string]any, error) {
		var res gen.CaptureSupplierInvoiceRes
		err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
			res, err = h.CaptureSupplierInvoice(ctx, &gen.SupplierInvoiceCaptureMultipart{LegalEntityID: e.DE, Document: gen.NewOptString(doc)}, gen.CaptureSupplierInvoiceParams{})
			return err
		})
		if err != nil {
			return nil, err
		}
		inv, _ := e.Must(200, "GET", res.(*gen.OperationHeaders).Response.Resource.Value, "", "")
		return inv, nil
	}
	pdf := "%PDF-1.7\n1 0 obj << /Type /Catalog >> endobj\ntrailer\n%%EOF"
	inv, err := capture(with(extractor{e: extraction()}, store), pdf)
	if err != nil {
		t.Fatal(err)
	}
	r := e.API.Do(apitest.Req{Method: "GET", Path: "/v1/supplier-invoices/" + inv["id"].(string) + "/document", Tenant: e.Tenant.ID, As: e.Tenant.Human})
	if r.Code != 200 {
		t.Fatalf("document status: got %d, want 200: %s", r.Code, r.Body)
	}
	if got := r.Body.String(); got != pdf {
		t.Errorf("document body: got %q, want %q", got, pdf)
	}
	if got := r.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("document content type: got %q, want application/octet-stream", got)
	}
	keys, _ := filepath.Glob(filepath.Join(root, "tenants", e.Tenant.ID.String(), "documents", "*"))
	if len(keys) != 2 { // the object and its sidecar
		t.Fatalf("stored: %v", keys)
	}
	content, ct, err := store.Get(context.Background(), strings.TrimPrefix(filepath.ToSlash(strings.TrimSuffix(keys[0], ".meta")), filepath.ToSlash(root)+"/"))
	if err != nil {
		t.Fatalf("read the stored document: %v", err)
	}
	if string(content) != pdf {
		t.Errorf("stored content: got %q, want %q", content, pdf)
	}
	if ct != "application/pdf" {
		t.Errorf("stored content type: got %q, want application/pdf", ct)
	}
	// Agents with access to the company may read documents.
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	r = e.API.Do(apitest.Req{Method: "GET", Path: "/v1/supplier-invoices/" + inv["id"].(string) + "/document", Tenant: e.Tenant.ID, As: e.Tenant.Agent})
	if r.Code != 200 {
		t.Fatalf("agent: %d", r.Code)
	}
	// An invoice entered by hand has no document; a capture made before object storage neither.
	manual, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "M-1", "1.00", "0.00", accountItem(e.expense, "1.00")), "")
	e.Problem(404, "request.not_found", "GET", "/v1/supplier-invoices/"+id(manual)+"/document", "", "")
	e.Problem(404, "request.not_found", "GET", "/v1/supplier-invoices/01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d01/document", "", "")
	before := e.API
	e.API = apitest.New(t)
	legacy, err := capture(with(extractor{e: changed("CAP-L")}, nil), "%PDF legacy")
	if err != nil {
		t.Fatal(err)
	}
	e.Problem(404, "request.not_found", "GET", "/v1/supplier-invoices/"+legacy["id"].(string)+"/document", "", "")
	// A server without object storage cannot hand out a stored document.
	e.Problem(503, "platform.unavailable", "GET", "/v1/supplier-invoices/"+inv["id"].(string)+"/document", "", "")
	e.API = before
	// A document whose stored bytes differ from the hash is not handed out; a missing object is 404.
	path := strings.TrimSuffix(keys[0], ".meta")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("make the document writable: %v", err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatalf("tamper with the document: %v", err)
	}
	e.Problem(409, "payables.document_changed", "GET", "/v1/supplier-invoices/"+inv["id"].(string)+"/document", "", "")
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the document: %v", err)
	}
	e.Problem(404, "request.not_found", "GET", "/v1/supplier-invoices/"+inv["id"].(string)+"/document", "", "")
	// A store that does not answer: 503 on capture and on download.
	broken := failingStore{err: errors.New("store down")}
	if _, err := capture(with(extractor{e: changed("CAP-B")}, broken), "%PDF broken"); problem.From(err).Status != 503 {
		t.Fatalf("capture with a broken store: %v", err)
	}
	e.API = apitest.NewWith(t, func(cfg *app.Config) { cfg.Objects = broken })
	e.Problem(503, "platform.unavailable", "GET", "/v1/supplier-invoices/"+inv["id"].(string)+"/document", "", "")
	e.API = before
	// A document stored by a capture that then failed is found there on the next capture.
	if _, err := capture(with(extractor{err: errors.New("model unavailable")}, store), "%PDF retried"); err == nil {
		t.Fatal("extraction failure captured")
	}
	retried, err := capture(with(extractor{e: changed("CAP-R")}, store), "%PDF retried")
	if err != nil {
		t.Fatalf("retry after a failed extraction: %v", err)
	}
	if _, _, err := with(nil, store).Document(context.Background(), e.DE); err == nil {
		t.Error("document outside a unit of work")
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "document", func(ctx context.Context) error {
		_, _, err := with(nil, store).Document(ctx, uuid.MustParse(retried["id"].(string)))
		return err
	})
}

// changed is an extraction of another invoice.
func changed(number string) *capture.Extraction {
	x := extraction()
	x.InvoiceNumber = number
	return x
}
