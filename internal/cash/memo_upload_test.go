package cash_test

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// upload posts a CSV file under a batch key as the client's principal and
// returns the status and body.
func (e memoEnv) upload(t *testing.T, as pgtest.Actor, key, file string) (int, map[string]any) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for _, f := range [][2]string{{"legal_entity_id", e.le.String()}, {"batch_key", key}, {"file", file}} {
		if err := w.WriteField(f[0], f[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r := e.API.Do(apitest.Req{Method: "POST", Path: "/v1/memo-record-uploads", Body: body.String(), Tenant: e.Tenant.ID, As: as, Key: apitest.NewKey(),
		Header: http.Header{"Content-Type": {w.FormDataContentType()}}})
	return r.Code, apitest.JSON(t, r)
}

// uploaded uploads as the person and returns the result's counts.
func (e memoEnv) uploaded(t *testing.T, key, file string) string {
	t.Helper()
	code, m := e.upload(t, e.Tenant.Human, key, file)
	if code != 200 {
		t.Fatalf("upload: got %d %v, want 200", code, m)
	}
	return fmt.Sprintf("rows %v created %v changed %v unchanged %v cancelled %v kept %v", m["rows"], m["created"], m["changed"], m["unchanged"], m["cancelled"],
		m["kept_realised"])
}

// batchOf lists the batch's records as "reference status amount".
func (e memoEnv) batchOf(t *testing.T, key string) []string {
	t.Helper()
	p, _ := e.Must(200, "GET", "/v1/memo-records?batch_key="+key, "", "")
	var out []string
	for _, it := range p["items"].([]any) {
		m := it.(map[string]any)
		out = append(out, fmt.Sprint(m["reference"], " ", m["status"], " ", amount(m["amount"]), " ", m["house_bank_id"] != nil, " ", m["origin"]))
	}
	return out
}

const header = "reference,value_date,amount,currency,liquidity_item,certainty,house_bank,expires_on,description\n"

// TestUploadMemoRecords: a batch creates its records; the same file again
// changes nothing and publishes nothing; a later file of the key changes,
// creates and cancels by reference, keeping realised records as they are
// and making cancelled ones active again (ADR-0051 §7).
func TestUploadMemoRecords(t *testing.T) {
	e := setupMemo(t)
	first := "\xEF\xBB\xBF" + header + "INV-1,2026-04-21,100.00,USD,REC_CUSTOMERS,,USD,,Northwind\nINV-2,2026-04-22,200.00,USD,REC_CUSTOMERS,confirmed,,2026-05-31,\n" +
		"INV-3,2026-04-23,-30.00,CAD,TAX,planned,MAIN,,\n"
	want(t, "first", e.uploaded(t, "billing-04", first), "rows 3 created 3 changed 0 unchanged 0 cancelled 0 kept 0")
	want(t, "batch", e.batchOf(t, "billing-04"), []string{"INV-1 active 100.00 USD true upload", "INV-2 active 200.00 USD false upload",
		"INV-3 active -30.00 CAD true upload"})
	want(t, "again", e.uploaded(t, "billing-04", first), "rows 3 created 0 changed 0 unchanged 3 cancelled 0 kept 0")
	want(t, "events after the same file", len(apitest.Published(t, e.Tenant, outbox.Event{Context: "cash", Name: "memo_record_changed"})), 3)

	second := "amount,reference,value_date,currency,liquidity_item\n120.00,INV-1,2026-04-21,USD,REC_CUSTOMERS\n-30.00,INV-3,2026-04-23,CAD,TAX\n" +
		"400.00,INV-4,2026-04-24,USD,REC_CUSTOMERS\n"
	want(t, "second", e.uploaded(t, "billing-04", second), "rows 3 created 1 changed 2 unchanged 0 cancelled 1 kept 0")
	want(t, "batch after the second", e.batchOf(t, "billing-04"), []string{"INV-1 active 120.00 USD false upload", "INV-2 cancelled 200.00 USD false upload",
		"INV-3 active -30.00 CAD false upload", "INV-4 active 400.00 USD false upload"})

	p, _ := e.Must(200, "GET", "/v1/memo-records?batch_key=billing-04&order_by=value_date", "", "")
	inv1 := p["items"].([]any)[0].(map[string]any)
	e.Must(200, "POST", path(inv1, "realise"), ``, fmt.Sprintf(`"%v"`, inv1["version"]))
	third := "reference,value_date,amount,currency,liquidity_item\nINV-1,2026-04-21,130.00,USD,REC_CUSTOMERS\nINV-2,2026-04-22,200.00,USD,REC_CUSTOMERS\n"
	want(t, "third", e.uploaded(t, "billing-04", third), "rows 2 created 0 changed 1 unchanged 0 cancelled 2 kept 1")
	want(t, "batch after the third", e.batchOf(t, "billing-04"), []string{"INV-1 realised 120.00 USD false upload", "INV-2 active 200.00 USD false upload",
		"INV-3 cancelled -30.00 CAD false upload", "INV-4 cancelled 400.00 USD false upload"})
	want(t, "the third again", e.uploaded(t, "billing-04", third), "rows 2 created 0 changed 0 unchanged 1 cancelled 0 kept 1")
	want(t, "another key", e.uploaded(t, "billing-05", "reference,value_date,amount,currency,liquidity_item\nINV-1,2026-04-21,1.00,USD,REC_CUSTOMERS\n"),
		"rows 1 created 1 changed 0 unchanged 0 cancelled 0 kept 0")
	want(t, "an empty file cancels the key's records", e.uploaded(t, "billing-05", "reference,value_date,amount,currency,liquidity_item\n"),
		"rows 0 created 0 changed 0 unchanged 0 cancelled 1 kept 0")
}

// TestUploadMemoRecordsRefusals: a file whose header or rows are wrong
// writes nothing and names each wrong field by its line; agents do not
// upload (ADR-0051 §7).
func TestUploadMemoRecordsRefusals(t *testing.T) {
	e := setupMemo(t)
	head := "reference,value_date,amount,currency,liquidity_item,house_bank,expires_on,description\n"
	for name, c := range map[string]struct{ file, fields string }{
		"rows": {head + "INV-1,2026-13-01,100.00,USD,REC_CUSTOMERS,,,\n" + "INV-1,2026-04-21,100.0,USD,REC_CUSTOMERS,NOPE,2026-04-3x,\n" +
			"INV-3,2026-04-21,-1.00,USD,REC_CUSTOMERS,,,\n" + ",2026-04-21,1.00,XXZ,REC_CUSTOMERS,,," + strings.Repeat("d", 501) + "\n" +
			"INV-5,2026-04-21,1.00,USD,REC_CUSTOMERS,MAIN,,\n",
			"rows[2].value_date rows[3].expires_on rows[3].house_bank rows[3].reference rows[4].liquidity_item rows[5].reference rows[5].description " +
				"rows[6].house_bank"},
		"a wrong amount":          {head + "INV-1,2026-04-21,100.0,USD,REC_CUSTOMERS,,,\n", "rows[2].amount"},
		"an unknown currency":     {head + "INV-1,2026-04-21,1.00,XXZ,REC_CUSTOMERS,,,\n", "rows[2].currency"},
		"an unknown column":       {"reference,value_date,amount,currency,liquidity_item,colour\n", "file"},
		"a column twice":          {"reference,value_date,amount,currency,liquidity_item,amount\n", "file"},
		"a missing column":        {"reference,value_date,amount,currency\n", "file"},
		"no header":               {"", "file"},
		"a row of other columns":  {head + "INV-1,2026-04-21\n", "file"},
		"a quote out of place":    {head + "INV-1,2026-04-21,1.00,USD,REC\"CUSTOMERS,,,\n", "file"},
		"not UTF-8":               {head + "INV-1,2026-04-21,1.00,USD,REC_CUSTOMERS,,,\xff\n", "file"},
		"over a megabyte":         {head + strings.Repeat("x", 1<<20), "file"},
		"over five thousand rows": {head + strings.Repeat("INV-1,2026-04-21,1.00,USD,REC_CUSTOMERS,,,\n", 5001), "file"},
	} {
		t.Run(name, func(t *testing.T) {
			code, p := e.upload(t, e.Tenant.Human, "billing-04", c.file)
			want(t, "refused", fmt.Sprint(code, " ", p["code"]), "422 cash.memo_upload_invalid")
			if c.fields == "file" {
				want(t, "field", fieldOf(p), "file")
				return
			}
			want(t, "fields", fields(p), c.fields)
		})
	}
	want(t, "nothing written", e.batchOf(t, "billing-04"), []string(nil))
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	code, p := e.upload(t, e.Tenant.Agent, "billing-04", head)
	want(t, "an agent", fmt.Sprint(code, " ", p["code"]), "403 identity.agent_not_allowed")
	other := e.Reader(t, "reader of 1000", e.US)
	code, _ = e.upload(t, other, "billing-04", head)
	want(t, "a legal entity the caller cannot act in", code, 404)
}
