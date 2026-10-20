package payments_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore/blobstoretest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

var statementPerms = []string{"payments.statement.read", "payments.statement.import"}

// statementSetup is the package setup with the permissions on statements
// and an object store that encrypts and locks for an hour.
func statementSetup(t *testing.T) (env, *blobstoretest.Server) {
	t.Helper()
	store := blobstoretest.New(t)
	objects := store.Store()
	objects.Encryption = blobstore.Encryption{Algorithm: blobstore.SSEKMS}
	return setupWith(t, statementPerms, func(cfg *app.Config) { cfg.Objects, cfg.Retention = objects, time.Hour }), store
}

// mtStatement is an MT940 statement of house bank MAIN of company 2000
// (DE89370400440532013000, EUR) with the balances and :61: lines given.
func mtStatement(ref, seq, opening, openDate, closing, closeDate string, lines ...string) string {
	return ":20:" + ref + "\n:25:DE89370400440532013000\n:28C:" + seq + "\n:60F:C" + openDate + "EUR" + opening + "\n" +
		strings.Join(lines, "") + ":62F:C" + closeDate + "EUR" + closing + "\n"
}

// upload posts the file for the house bank and returns the status and body.
func (e env) upload(t *testing.T, bank, format, file string) (int, map[string]any) {
	t.Helper()
	part := func(name, value string) string {
		return "--b\r\nContent-Disposition: form-data; name=\"" + name + "\"\r\n\r\n" + value + "\r\n"
	}
	r := e.API.Do(apitest.Req{Method: "POST", Path: "/v1/bank-statements", Tenant: e.Tenant.ID, As: e.Client.As, Key: apitest.NewKey(),
		Header: map[string][]string{"Content-Type": {"multipart/form-data; boundary=b"}},
		Body:   part("house_bank_id", bank) + part("format", format) + part("file", file) + "--b--\r\n"})
	return r.Code, apitest.JSON(t, r)
}

// agent is e acting as the tenant's agent.
func (e env) agent() env {
	e.Client.As = e.Tenant.Agent
	return e
}

func (e env) imported(t *testing.T, format, file string) []any {
	t.Helper()
	code, body := e.upload(t, e.bank.String(), format, file)
	if code != 201 {
		t.Fatalf("import: got %d %v, want 201", code, body)
	}
	return body["statements"].([]any)
}

func field(v any, path ...string) any {
	for _, p := range path {
		v = v.(map[string]any)[p]
	}
	return v
}

// TestImportStatements: a file's statements are imported with their lines,
// each after the house bank's statement before it; the same file again is
// the first import, and another file with an imported statement is
// refused (ADR-0047).
func TestImportStatements(t *testing.T) {
	e, _ := statementSetup(t)
	file := mtStatement("S1", "00001/001", "1000,00", "261019", "989,50", "261020",
		":61:2610201020D10,50NCHGNONREF//B1\n:86:/EREF/E2E-1/NAME/Bank AG/REMI/FEES\n") +
		mtStatement("S2", "00002/001", "989,50", "261020", "1089,50", "261021", ":61:261021C100,00NTRFREF2//B2\n")
	got := e.imported(t, "mt940", file)
	if len(got) != 2 {
		t.Fatalf("statements: got %d, want 2", len(got))
	}
	first, second := got[0].(map[string]any), got[1].(map[string]any)
	for name, c := range map[string]struct{ got, want any }{
		"format":             {first["format"], "mt940"},
		"status":             {first["status"], "imported"},
		"sequence":           {first["sequence"], "00001/001"},
		"opening":            {field(first, "opening_balance", "amount"), "1000.00"},
		"closing":            {field(first, "closing_balance", "amount"), "989.50"},
		"line amount":        {field(first["lines"].([]any)[0], "amount", "amount"), "-10.50"},
		"line code":          {field(first["lines"].([]any)[0], "transaction_code"), "NCHG"},
		"line status":        {field(first["lines"].([]any)[0], "status"), "unmatched"},
		"end-to-end id":      {field(first["lines"].([]any)[0].(map[string]any)["transactions"].([]any)[0], "end_to_end_id"), "E2E-1"},
		"first has none":     {first["previous_statement_id"], nil},
		"second after first": {second["previous_statement_id"], first["id"]},
		"position":           {second["file_position"], float64(2)},
		"hash":               {second["file_sha256"], fmt.Sprintf("%x", sha256.Sum256([]byte(file)))},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", name, c.got, c.want)
		}
	}
	if n := len(events(t, e.Tenant, "bank_statement_imported")); n != 2 {
		t.Errorf("events: got %d, want 2", n)
	}
	t.Run("the same file again", func(t *testing.T) {
		code, body := e.in(t).upload(t, e.bank.String(), "mt940", file)
		if code != 200 || field(body["statements"].([]any)[1], "id") != second["id"] {
			t.Errorf("again: got %d %v, want 200 with the same statements", code, body)
		}
		if n := len(events(t, e.Tenant, "bank_statement_imported")); n != 2 {
			t.Errorf("events after the same file: got %d, want 2", n)
		}
	})
	t.Run("another file with an imported statement", func(t *testing.T) {
		code, body := e.in(t).upload(t, e.bank.String(), "mt940", mtStatement("S2", "00002/001", "989,50", "261020", "989,50", "261021"))
		if code != 409 || body["code"] != "payments.statement_conflict" {
			t.Errorf("conflict: got %d %v, want 409 payments.statement_conflict", code, body)
		}
	})
	t.Run("read", func(t *testing.T) {
		one, tag := e.in(t).Must(200, "GET", "/v1/bank-statements/"+id(first), "", "")
		if len(one["lines"].([]any)) != 1 || tag != `"1"` {
			t.Errorf("get: got %v %s, want its one line and version 1", one, tag)
		}
		page, _ := e.in(t).Must(200, "GET", "/v1/bank-statements?house_bank_id="+e.bank.String()+"&status=imported&page_size=1", "", "")
		if items := page["items"].([]any); len(items) != 1 || field(items[0], "id") != first["id"] || field(items[0], "lines") != nil {
			t.Errorf("list: got %v, want the first statement without lines", page)
		}
		e.in(t).Problem(404, "request.not_found", "GET", "/v1/bank-statements/"+e.expense.String(), "", "")
	})
}

// TestStatementGaps: a statement whose opening balance is not the closing
// balance of the bank's statement before it is imported in gap; the
// missing statement, when it arrives, closes the gap.
func TestStatementGaps(t *testing.T) {
	e, _ := statementSetup(t)
	first := e.imported(t, "mt940", mtStatement("G1", "1", "500,00", "261001", "500,00", "261001"))[0].(map[string]any)
	third := e.imported(t, "mt940", mtStatement("G3", "3", "700,00", "261003", "700,00", "261003"))[0].(map[string]any)
	if third["status"] != "gap" || third["previous_statement_id"] != first["id"] {
		t.Fatalf("after a missing statement: got %v, want gap after the first", third)
	}
	if third["lines"] == nil {
		t.Errorf("lines of a statement without any: got none, want []")
	}
	second := e.imported(t, "mt940", mtStatement("G2", "2", "500,00", "261002", "700,00", "261002", ":61:261002C200,00NTRFNONREF\n"))[0].(map[string]any)
	if second["status"] != "imported" || second["previous_statement_id"] != first["id"] {
		t.Errorf("the missing statement: got %v, want imported after the first", second)
	}
	now, _ := e.Must(200, "GET", "/v1/bank-statements/"+id(third), "", "")
	if now["status"] != "imported" || now["previous_statement_id"] != second["id"] || now["version"] != float64(2) {
		t.Errorf("the statement in gap: got %v, want imported after the missing one", now)
	}
	if n := len(events(t, e.Tenant, "bank_statement_gap_closed")); n != 1 {
		t.Errorf("gap closed events: got %d, want 1", n)
	}
	// An older statement imported later is the first one's previous
	// statement: a gap opens where its closing balance does not lead to it.
	zero := e.imported(t, "mt940", mtStatement("G0", "0", "1,00", "260901", "1,00", "260901"))[0].(map[string]any)
	if again, _ := e.Must(200, "GET", "/v1/bank-statements/"+id(first), "", ""); again["status"] != "gap" || again["previous_statement_id"] != zero["id"] {
		t.Errorf("first statement after an older one that does not lead to it: got %v, want gap after it", again)
	}
	if n := len(events(t, e.Tenant, "bank_statement_gap_found")); n != 1 {
		t.Errorf("gap found events: got %d, want 1", n)
	}
	// A second statement of the same day comes after the first and leaves it.
	d1 := e.imported(t, "mt940", mtStatement("D1", "1", "300,00", "261201", "300,00", "261201"))[0].(map[string]any)
	d2 := e.imported(t, "mt940", mtStatement("D2", "2", "300,00", "261201", "300,00", "261201"))[0].(map[string]any)
	if again, _ := e.Must(200, "GET", "/v1/bank-statements/"+id(d1), "", ""); again["version"] != float64(1) || d2["previous_statement_id"] != d1["id"] {
		t.Errorf("statements of one day: got first %v and second after %v, want the first unchanged and the second after it", again, d2["previous_statement_id"])
	}
	// One that leads to it keeps it imported.
	e.imported(t, "mt940", mtStatement("H0", "0", "200,00", "261102", "200,00", "261102"))
	h1 := e.imported(t, "mt940", mtStatement("H1", "1", "200,00", "261105", "200,00", "261105"))[0].(map[string]any)
	h := e.imported(t, "mt940", mtStatement("H", "9", "200,00", "261103", "200,00", "261104"))[0].(map[string]any)
	if again, _ := e.Must(200, "GET", "/v1/bank-statements/"+id(h1), "", ""); again["status"] != "imported" || again["previous_statement_id"] != h["id"] {
		t.Errorf("a statement after an older one that leads to it: got %v, want imported after it", again)
	}
	if n := len(events(t, e.Tenant, "bank_statement_gap_found")) + len(events(t, e.Tenant, "bank_statement_gap_closed")); n != 2 {
		t.Errorf("gap events after a relink without change: got %d, want 2", n)
	}
}

// TestStatementCamt: a camt.053 file is imported in its version.
func TestStatementCamt(t *testing.T) {
	e, _ := statementSetup(t)
	doc := `<?xml version="1.0" encoding="UTF-8"?>
<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.02"><BkToCstmrStmt><GrpHdr><MsgId>MSG-1</MsgId></GrpHdr>
<Stmt><Id>C1</Id><ElctrncSeqNb>7</ElctrncSeqNb><Acct><Id><IBAN>DE89370400440532013000</IBAN></Id><Ccy>EUR</Ccy></Acct>
<Bal><Tp><CdOrPrtry><Cd>OPBD</Cd></CdOrPrtry></Tp><Amt Ccy="EUR">10.00</Amt><CdtDbtInd>CRDT</CdtDbtInd><Dt><Dt>2026-10-20</Dt></Dt></Bal>
<Bal><Tp><CdOrPrtry><Cd>CLBD</Cd></CdOrPrtry></Tp><Amt Ccy="EUR">10.00</Amt><CdtDbtInd>CRDT</CdtDbtInd><Dt><Dt>2026-10-20</Dt></Dt></Bal>
</Stmt></BkToCstmrStmt></Document>`
	s := e.imported(t, "camt.053", doc)[0].(map[string]any)
	if s["format"] != "camt.053.001.02" || s["message_id"] != "MSG-1" {
		t.Errorf("camt.053: got %v, want version .001.02 and its message id", s)
	}
}

// TestStatementRefusals: what an import refuses, and why.
func TestStatementRefusals(t *testing.T) {
	e, _ := statementSetup(t)
	ok := mtStatement("R1", "1", "1,00", "261001", "1,00", "261001")
	for _, c := range []struct {
		name, bank, format, file, code string
	}{
		{"another format", e.bank.String(), "bai2", ok, "request.validation_failed"},
		{"an empty file", e.bank.String(), "mt940", "", "request.validation_failed"},
		{"a file too large", e.bank.String(), "mt940", ok + strings.Repeat(":86:x\n", payments.MaxStatementFile/6), "request.validation_failed"},
		{"an unreadable file", e.bank.String(), "mt940", strings.Replace(ok, ":62F:C261001EUR1,00", ":62F:C261001EUR2,00", 1), "payments.statement_unreadable"},
		{"another account", e.bank.String(), "mt940", strings.Replace(ok, "DE89370400440532013000", "GB33BUKB20201555555555", 1), "payments.statement_not_of_bank"},
		{"another currency", e.bank.String(), "mt940", strings.ReplaceAll(ok, "EUR", "USD"), "payments.statement_not_of_bank"},
		{"no such house bank", e.expense.String(), "mt940", ok, "request.reference_not_found"},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, body := e.in(t).upload(t, c.bank, c.format, c.file)
			if code != 422 || body["code"] != c.code {
				t.Errorf("got %d %v, want 422 %s", code, body, c.code)
			}
		})
	}
	t.Run("an order the list does not have", func(t *testing.T) {
		e.in(t).Problem(400, "request.invalid_order_by", "GET", "/v1/bank-statements?order_by=amount", "", "")
	})
	t.Run("an agent", func(t *testing.T) {
		if code, body := (env{Env: e.Env, bank: e.bank}).agent().upload(t, e.bank.String(), "mt940", ok); code != 403 || body["code"] != "identity.agent_not_allowed" {
			t.Errorf("got %d %v, want 403 identity.agent_not_allowed", code, body)
		}
	})
	t.Run("the line of what cannot be read", func(t *testing.T) {
		_, body := e.in(t).upload(t, e.bank.String(), "mt940", strings.Replace(ok, ":60F:C261001EUR1,00", ":60F:C261001EUR1.00", 1))
		if msg := field(body["errors"].([]any)[0], "message"); !strings.HasPrefix(fmt.Sprint(msg), "line 4: ") {
			t.Errorf("message: got %q, want it to start with line 4", msg)
		}
	})
	t.Run("an IBAN that does not open", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE payments.house_bank SET iban_cipher = '\x00', version = version + 1 WHERE id = $1`, e.bank)
		if code, body := e.in(t).upload(t, e.bank.String(), "mt940", ok); code != 500 {
			t.Errorf("got %d %v, want 500", code, body)
		}
	})
}

// TestStatementDatabaseFailures: every statement of an import and of the
// reads fails in turn, and none runs outside a unit of work.
func TestStatementDatabaseFailures(t *testing.T) {
	e, store := statementSetup(t)
	h := endpoints(nil)
	h.Objects = store.Store()
	s := e.imported(t, "mt940", mtStatement("F1", "1", "1,00", "261001", "1,00", "261001"))[0].(map[string]any)
	n := 0
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "import", func(ctx context.Context) error {
		n++ // a new file each time, after the first and closing a gap, so every statement runs
		_, _, _, err := h.ImportStatements(ctx, e.bank, "mt940", []byte(mtStatement(fmt.Sprintf("F%d", n+1), "2", "1,00", "261002", "1,00", "261002")))
		return err
	})
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "import again", func(ctx context.Context) error {
		_, _, _, err := h.ImportStatements(ctx, e.bank, "mt940", []byte(mtStatement("F1", "1", "1,00", "261001", "1,00", "261001")))
		return err
	})
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "get", func(ctx context.Context) error {
		_, err := payments.GetStatement(ctx, uuidOf(id(s)))
		return err
	})
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "file", func(ctx context.Context) error {
		_, err := h.StatementFile(ctx, uuidOf(id(s)))
		return err
	})
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "list", func(ctx context.Context) error {
		_, err := h.ListBankStatements(ctx, gen.ListBankStatementsParams{})
		return err
	})
	for name, err := range map[string]error{
		"import": func() error {
			_, _, _, err := h.ImportStatements(context.Background(), e.bank, "mt940", nil)
			return err
		}(),
		"get": func() error { _, err := payments.GetStatement(context.Background(), e.bank); return err }(),
		"list": func() error {
			_, err := h.ListBankStatements(context.Background(), gen.ListBankStatementsParams{})
			return err
		}(),
		"handler": func() error {
			_, err := h.GetBankStatement(context.Background(), gen.GetBankStatementParams{ID: e.bank})
			return err
		}(),
		"import handler": func() error {
			_, err := h.ImportBankStatements(context.Background(), &gen.BankStatementUploadMultipart{}, gen.ImportBankStatementsParams{})
			return err
		}(),
		"file": func() error {
			_, err := h.GetBankStatementFile(context.Background(), gen.GetBankStatementFileParams{ID: e.bank})
			return err
		}(),
	} {
		t.Run(name, func(t *testing.T) { pgtest.WantErr(t, err, "") })
	}
}

// TestStatementFile: the file a statement was imported from is kept as
// evidence, written once under the tenant and its hash, encrypted and
// locked, and downloaded as it came; without the store nothing is
// imported (ADR-0047).
func TestStatementFile(t *testing.T) {
	e, store := statementSetup(t)
	file := mtStatement("K1", "1", "1,00", "261001", "1,00", "261001")
	s := e.imported(t, "mt940", file)[0].(map[string]any)
	key := fmt.Sprintf("tenants/%s/bank-statements/%x", e.Tenant.ID, sha256.Sum256([]byte(file)))
	o, ok := store.Object(key)
	if !ok {
		t.Fatalf("object %s: not stored; stored %v", key, store.Keys())
	}
	for name, c := range map[string]struct{ got, want any }{
		"content":      {string(o.Content), file},
		"content type": {o.ContentType, "text/plain; charset=utf-8"},
		"lock mode":    {o.LockMode, "COMPLIANCE"},
		"locked":       {o.RetainUntil != "", true},
		"encryption":   {o.Encryption, blobstore.SSEKMS},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", name, c.got, c.want)
		}
	}
	download := func(e env) *httptest.ResponseRecorder {
		return e.API.Do(apitest.Req{Method: "GET", Path: "/v1/bank-statements/" + id(s) + "/file", Tenant: e.Tenant.ID, As: e.Client.As})
	}
	t.Run("downloaded as it came", func(t *testing.T) {
		if r := download(e.in(t)); r.Code != 200 || r.Body.String() != file {
			t.Errorf("got %d %q, want 200 and the file", r.Code, r.Body.String())
		}
		e.in(t).Problem(404, "request.not_found", "GET", "/v1/bank-statements/"+e.expense.String()+"/file", "", "")
	})
	t.Run("imported again after its rows went, the kept file is the same", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM payments.bank_statement_line WHERE statement_id = $1`, uuidOf(id(s)))
		pgtest.Exec(t, `UPDATE payments.bank_statement SET file_sha256 = decode(repeat('00', 32), 'hex'), statement_key = 'gone', version = version + 1 WHERE id = $1`, uuidOf(id(s)))
		if again := e.in(t).imported(t, "mt940", file)[0].(map[string]any); again["id"] == s["id"] {
			t.Errorf("again: got the same statement %v, want a new one", again["id"])
		}
	})
	t.Run("the store fails", func(t *testing.T) {
		store.Fail = 500
		defer func() { store.Fail = 0 }()
		if code, body := e.in(t).upload(t, e.bank.String(), "mt940", mtStatement("K2", "2", "1,00", "261002", "1,00", "261002")); code != 503 {
			t.Errorf("import: got %d %v, want 503", code, body)
		}
		if r := download(e.in(t)); r.Code != 503 {
			t.Errorf("download: got %d %s, want 503", r.Code, r.Body.String())
		}
	})
	t.Run("no object store", func(t *testing.T) {
		bare := e.in(t)
		bare.API = apitest.NewWith(t, func(*app.Config) {})
		if code, body := bare.upload(t, e.bank.String(), "mt940", mtStatement("K3", "3", "1,00", "261003", "1,00", "261003")); code != 503 {
			t.Errorf("import: got %d %v, want 503", code, body)
		}
		if r := download(bare); r.Code != 503 {
			t.Errorf("download: got %d %s, want 503", r.Code, r.Body.String())
		}
	})
}
