package reader

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// taxLine is a gl_lines row as the extractor wrote it with the tax columns
// and before the centers' (batch 95).
type taxLine struct {
	TenantID        uuid.UUID    `parquet:"tenant_id,uuid"`
	DocumentID      uuid.UUID    `parquet:"document_id,uuid"`
	LineNo          int32        `parquet:"line_no"`
	LedgerID        uuid.UUID    `parquet:"ledger_id,uuid"`
	Company         string       `parquet:"company"`
	PostingDate     int32        `parquet:"posting_date,date"`
	Account         string       `parquet:"account"`
	DocumentType    string       `parquet:"document_type"`
	Vendor          *string      `parquet:"vendor,optional"`
	AmountMinor     money.Minor  `parquet:"amount_minor"`
	Currency        string       `parquet:"currency"`
	Exponent        int32        `parquet:"exponent"`
	PostedXID       uint64       `parquet:"posted_xid"`
	TaxJurisdiction *string      `parquet:"tax_jurisdiction,optional"`
	TaxBaseMinor    *money.Minor `parquet:"tax_base_minor,optional"`
}

// TestCenterColumns: the journal's lines are grouped and filtered by their
// cost center, profit center and segment, a line without one under none.
// A file written before the columns existed does not bind, so the store
// answers from the database until the extractor has written the journal
// again (ADR-0044 As built T56i1).
func TestCenterColumns(t *testing.T) {
	e := setup(t)
	cc, pc, seg := "1100", "PC-MTL", "CA"
	cost, sale := line(e.a, "1000", "2026-04-02", "610000", nil, 300), line(e.a, "1000", "2026-04-03", "400000", nil, -1000)
	cost.CostCenter, cost.ProfitCenter, cost.Segment = &cc, &pc, &seg
	sale.ProfitCenter, sale.Segment = &pc, &seg
	newer := encode(t, []glLine{cost, sale})
	m := e.manifs[e.a]
	gl := m.Datasets["gl_lines"]
	gl.Files = append(gl.Files, e.put(part(e.a, "gl_lines", "2026-04", newer), newer))
	m.Datasets["gl_lines"] = gl
	e.keys[e.a] = e.publish(e.a, m)
	// The tenant's other lines, adding up to 157 (1.57 EUR), carry no center.
	for name, c := range map[string]struct {
		plan Plan
		want map[string]money.Minor
	}{
		"by cost center": {Plan{Dataset: "gl_lines", Ledger: nilLedger, Aggregate: "sum", By: []string{"cost_center"}},
			map[string]money.Minor{"|EUR": -843, "1100|EUR": 300}},
		"by profit center and segment": {Plan{Dataset: "gl_lines", Ledger: nilLedger, Aggregate: "sum", By: []string{"profit_center", "segment"}},
			map[string]money.Minor{"||EUR": 157, "PC-MTL|CA|EUR": -700}},
		"of the cost center": {Plan{Dataset: "gl_lines", Ledger: nilLedger, Aggregate: "sum", Where: []Filter{{Column: "cost_center", Op: "in", Values: []string{cc}}}},
			map[string]money.Minor{"|EUR": 300}},
		"not of the segment": {Plan{Dataset: "gl_lines", Ledger: nilLedger, Aggregate: "sum", Where: []Filter{{Column: "segment", Op: "not_in", Values: []string{seg}}}},
			map[string]money.Minor{"|EUR": 157}},
	} {
		t.Run(name, func(t *testing.T) { wantSums(t, e.query(e.request(e.a, c.plan)), c.want) })
	}
	t.Run("a file written before them", func(t *testing.T) {
		older := encode(t, []taxLine{{TenantID: e.b, DocumentID: uuid.New(), LineNo: 1, Company: "1000", PostingDate: day("2026-05-01"),
			Account: "400000", DocumentType: "SA", AmountMinor: -5, Currency: "EUR", Exponent: 2, PostedXID: 7}})
		m := e.manifs[e.b]
		gl := m.Datasets["gl_lines"]
		gl.Files = append(gl.Files, e.put(part(e.b, "gl_lines", "2026-05", older), older))
		m.Datasets["gl_lines"] = gl
		byCenter := Plan{Dataset: "gl_lines", Ledger: nilLedger, Aggregate: "sum", By: []string{"cost_center"}}
		_, err := e.r.Query(context.Background(), Request{TenantID: e.b, ManifestKey: e.publish(e.b, m), Scope: everything, Plan: byCenter})
		if err == nil || !strings.Contains(err.Error(), `"cost_center"`) {
			t.Errorf("query: got %v, want the view not bound for the missing cost_center, so the store answers from the database", err)
		}
	})
}
