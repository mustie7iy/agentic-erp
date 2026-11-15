package endpoint_test

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
)

// TestQueryWorkbook (T95d): format=xlsx answers the result as an Excel
// workbook, each amount a number with its currency's decimals beside its
// currency; a retry with the Idempotency-Key replays the same bytes; a
// format the contract does not list is 422.
func TestQueryWorkbook(t *testing.T) {
	e := seeded(t, perms...)
	req := apitest.Req{Method: "POST", Path: "/v1/analytics-queries?format=xlsx", Body: `{"measures": ["revenue"], "by": ["company"], "order_by": "company"}`,
		Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}
	first := e.API.Do(req)
	if got := first.Header().Get("Content-Type"); first.Code != 200 || got != analytics.XLSXMediaType {
		t.Fatalf("workbook: got %d %s (%s), want 200 %s", first.Code, got, first.Body, analytics.XLSXMediaType)
	}
	sheet := sheetPart(t, first.Body.Bytes())
	for _, want := range []string{`<c r="A2" t="inlineStr"><is><t xml:space="preserve">1000</t></is></c><c r="B2" s="1"><v>800.00</v></c>`,
		`<c r="C2" t="inlineStr"><is><t xml:space="preserve">USD</t></is></c>`, `<c r="B3" s="1"><v>500.00</v></c>`} {
		if !strings.Contains(sheet, want) {
			t.Errorf("sheet: got %s, want %s", sheet, want)
		}
	}
	replay := e.API.Do(req)
	if replay.Code != 200 || replay.Header().Get("Idempotent-Replayed") != "true" || !bytes.Equal(replay.Body.Bytes(), first.Body.Bytes()) {
		t.Errorf("replay: got %d %v, want the same workbook replayed", replay.Code, replay.Header())
	}
	if code, body, _ := e.Do("POST", "/v1/analytics-queries?format=pdf", `{"measures": ["revenue"], "by": ["company"]}`, ""); code != 422 || body["code"] != "request.validation_failed" {
		t.Errorf("format pdf: got %d %v, want 422 request.validation_failed", code, body)
	}
}

// sheetPart is the workbook's sheet.
func sheetPart(t *testing.T, b []byte) string {
	t.Helper()
	z, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("workbook: %v", err)
	}
	f, err := z.Open("xl/worksheets/sheet1.xml")
	if err != nil {
		t.Fatalf("sheet: %v", err)
	}
	sheet, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("sheet: %v", err)
	}
	return string(sheet)
}
