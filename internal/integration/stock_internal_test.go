package integration

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/sap"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// stockTarget is company 2000 in EUR with valuation class 3000.
func stockTarget() target {
	return target{exponents: map[string]int{"EUR": 2}, entities: map[string]tenancy.Entity{"2000": {Code: "2000", LocalCurrency: "EUR"}},
		classes: map[string]map[string]config.InventoryAccounts{"2000": {"3000": {ValuationClass: "3000", Stock: "150000"}}}}
}

// stockExtract is plant P1 (storage locations S1, S2) and P2 (S3), each
// its own valuation area, with material M-1 at P1/S1 and P1/S2 and at
// P2/S3, and material NL (non-stock) at P1/S1, and the valuations given.
func stockExtract(labst map[string]string, rows ...sap.MBEW) *sap.Extract {
	x := &sap.Extract{T001W: []sap.T001W{{WERKS: "P1", NAME1: "One", BWKEY: "P1", BUKRS: "2000"}, {WERKS: "P2", NAME1: "Two", BWKEY: "P2", BUKRS: "2000"}},
		T001L: []sap.T001L{{WERKS: "P1", LGORT: "S1", LGOBE: "One"}, {WERKS: "P1", LGORT: "S2", LGOBE: "Two"}, {WERKS: "P2", LGORT: "S3", LGOBE: "Three"}},
		MARA:  []sap.MARA{{MATNR: "M-1", MTART: "ROH", MATKL: "STEEL", MEINS: "KG"}, {MATNR: "NL", MTART: "NLAG", MATKL: "MISC", MEINS: "KG"}},
		MAKT:  []sap.MAKT{{MATNR: "M-1", SPRAS: "E", MAKTX: "Steel"}, {MATNR: "NL", SPRAS: "E", MAKTX: "Non-stock"}},
		MARM:  []sap.MARM{{MATNR: "M-1", MEINH: "KG", UMREZ: "1", UMREN: "1"}, {MATNR: "NL", MEINH: "KG", UMREZ: "1", UMREN: "1"}},
		MARC:  []sap.MARC{{MATNR: "M-1", WERKS: "P1"}, {MATNR: "M-1", WERKS: "P2"}, {MATNR: "NL", WERKS: "P1"}},
		MARD: []sap.MARD{{MATNR: "M-1", WERKS: "P1", LGORT: "S1", LABST: labst["P1/S1"]}, {MATNR: "M-1", WERKS: "P1", LGORT: "S2", LABST: labst["P1/S2"]},
			{MATNR: "M-1", WERKS: "P2", LGORT: "S3", LABST: labst["P2/S3"]}, {MATNR: "NL", WERKS: "P1", LGORT: "S1", LABST: labst["NL"]}},
		MBEW: rows}
	return x
}

// moving is M-1's valuation in an area at a moving price of 2.50 EUR.
func moving(area, stock, value string) sap.MBEW {
	return sap.MBEW{MATNR: "M-1", BWKEY: area, VPRSV: "V", VERPR: "2.50", STPRS: "0", PEINH: "1", BKLAS: "3000", LBKUM: stock, SALK3: value}
}

// stocks plans the extract's stock against its plants and materials.
func stocks(x *sap.Extract) ([]sapStock, []string) {
	plants, _ := plantsOf(x)
	mats, _ := materialsOf(x, map[string]string{"KG": "mass"}, plants)
	got, issues := stocksOf(x, stockTarget(), plants, mats)
	var out []string
	for _, i := range issues {
		out = append(out, i.Severity+" "+i.Table+" "+i.Key+": "+i.Message)
	}
	return got, out
}

// TestStockShares: a moving-priced stock at two storage locations is
// shared out by quantity, the shares adding up to SAP's value; a standard
// one is valued at its price; a storage location holding none is left out.
func TestStockShares(t *testing.T) {
	std := sap.MBEW{MATNR: "M-1", BWKEY: "P2", VPRSV: "S", VERPR: "9.00", STPRS: "10.00", PEINH: "1", BKLAS: "3000", LBKUM: "6.000", SALK3: "60.00"}
	got, issues := stocks(stockExtract(map[string]string{"P1/S1": "60.000", "P1/S2": "40", "P2/S3": "6.000", "NL": "0"}, moving("P1", "100.000", "250.01"), std))
	if len(issues) != 0 {
		t.Fatalf("issues: got %v, want none", issues)
	}
	if len(got) != 2 {
		t.Fatalf("stocks: got %d, want 2", len(got))
	}
	cases := []struct {
		name, got, want string
	}{
		{"P1's shares", sharesText(got[0]), "S1 60 150.01, S2 40 100.00"},
		{"P1's control and value", got[0].control + " " + got[0].value.Plain(2) + " " + got[0].price.Plain(2), "moving_average 250.01 2.50"},
		{"P2's stock", sharesText(got[1]), "S3 6 0.00"},
		{"P2's control and price", got[1].control + " " + got[1].price.Plain(2), "standard 10.00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %q, want %q", c.got, c.want)
			}
		})
	}
}

func sharesText(s sapStock) string {
	var out []string
	for _, l := range s.locations {
		out = append(out, l.lgort+" "+l.quantity+" "+l.amount.Plain(2))
	}
	return strings.Join(out, ", ")
}

// TestStockQualityGate: what the platform cannot take over of a material
// that loads refuses the run; the stock of one that does not load is a
// warning.
func TestStockQualityGate(t *testing.T) {
	ok := map[string]string{"P1/S1": "100", "P2/S3": "0"}
	cases := []struct {
		name  string
		labst map[string]string
		rows  []sap.MBEW
		edit  func(x *sap.Extract)
		want  string
	}{
		{"twice", ok, []sap.MBEW{moving("P1", "100", "250.00"), moving("P1", "100", "250.00")}, nil, "error MBEW P1/M-1: the valuation is listed twice"},
		{"split", ok, []sap.MBEW{func() sap.MBEW { m := moving("P1", "100", "250.00"); m.BWTAR = "DOM"; return m }()}, nil,
			`it is split-valued (valuation type "DOM")`},
		{"price control", ok, []sap.MBEW{func() sap.MBEW { m := moving("P1", "100", "250.00"); m.VPRSV = "X"; return m }()}, nil,
			`price control "X" is neither S nor V`},
		{"class", ok, []sap.MBEW{func() sap.MBEW { m := moving("P1", "100", "250.00"); m.BKLAS = "9999"; return m }()}, nil,
			`valuation class "9999" is no class of company 2000's account determination`},
		{"price unit", ok, []sap.MBEW{func() sap.MBEW { m := moving("P1", "100", "250.00"); m.PEINH = "0"; return m }()}, nil,
			`price unit "0" is not a whole number from 1 to 10,000`},
		{"standard price zero", ok, []sap.MBEW{func() sap.MBEW { m := moving("P1", "100", "250.00"); m.VPRSV, m.STPRS = "S", "0.00"; return m }()}, nil,
			`price "0.00" is not an amount above zero in EUR`},
		{"value", ok, []sap.MBEW{moving("P1", "100", "x")}, nil, `value "x" is not an amount of zero or more in EUR`},
		{"stock", ok, []sap.MBEW{moving("P1", "1e2", "250.00")}, nil, `stock "1e2" is not a plain quantity`},
		{"storage location stock", map[string]string{"P1/S1": "-1"}, []sap.MBEW{moving("P1", "100", "250.00")}, nil,
			`the stock "-1" at storage location S1 (MARD LABST) is not a plain quantity`},
		{"sum", map[string]string{"P1/S1": "90"}, []sap.MBEW{moving("P1", "100", "250.00")}, nil,
			"its stock 100 (LBKUM) is not the sum of its storage locations' unrestricted stock (MARD LABST), 90"},
		{"deleted location", ok, []sap.MBEW{moving("P1", "100", "250.00")}, func(x *sap.Extract) { x.MARD[0].LVORM = sap.Deleted },
			"storage location S1, flagged for deletion (MARD LVORM), holds stock 100"},
		{"value without stock", map[string]string{}, []sap.MBEW{moving("P1", "0", "5.00")}, nil, `it has a value of 5.00 (SALK3) without stock`},
		{"area of two plants", ok, []sap.MBEW{moving("AREA", "100", "250.00")}, func(x *sap.Extract) { x.T001W[0].BWKEY, x.T001W[1].BWKEY = "AREA", "AREA" },
			"error MBEW AREA/M-1: valuation area AREA is the area of 2 plants of the scope (P1, P2)"},
		{"no valuation", ok, nil, nil, `error MARD M-1/P1/S1: the stock "100" of material M-1 has no valuation (MBEW) at plant P1`},
		{"non-stock with stock", map[string]string{"P1/S1": "100", "NL": "5"},
			[]sap.MBEW{moving("P1", "100", "250.00"), {MATNR: "NL", BWKEY: "P1", VPRSV: "V", VERPR: "1", PEINH: "1", BKLAS: "3000", LBKUM: "5", SALK3: "5.00"}}, nil,
			`error MBEW P1/NL: material NL is of a type that keeps no stock (non_stock) but SAP values stock of "5"`},
		{"not migrated", ok, []sap.MBEW{moving("P1", "100", "250.00"), func() sap.MBEW { m := moving("P2", "3", "7.50"); m.MATNR = "M-9"; return m }()}, nil,
			"warning MBEW P2/M-9: the stock of 3 M-9 (value 7.50, as extracted) at plant P2 is not taken over: the material is not migrated there"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			x := stockExtract(c.labst, c.rows...)
			if c.edit != nil {
				c.edit(x)
			}
			_, issues := stocks(x)
			if !slices.ContainsFunc(issues, func(s string) bool { return strings.Contains(s, c.want) }) {
				t.Errorf("issues: got %v, want one holding %s", issues, c.want)
			}
		})
	}
}

// TestStockSharesOutOfRange: a value whose shares cannot be told is
// refused rather than wrapped.
func TestStockSharesOutOfRange(t *testing.T) {
	s := sapStock{control: "moving_average", value: money.Minor(1 << 62), quantity: money.ParseRat("1"),
		locations: []sapStockAt{{lgort: "S1", quantity: "4"}, {lgort: "S2", quantity: "4"}}}
	if reasons := (&stockPlan{}).shares(&s); len(reasons) != 1 {
		t.Errorf("reasons: got %v, want the shares refused", reasons)
	}
}

// lockedStock is a Stock that answers LockNumbers with day, or with
// refused, and records what TakeOver is given.
type lockedStock struct {
	Stock
	day     time.Time
	refused error
	locked  []uuid.UUID
}

func (f *lockedStock) LockNumbers(_ context.Context, company uuid.UUID) (time.Time, error) {
	f.locked = append(f.locked, company)
	return f.day, f.refused
}

// TestTakeOverOnTheLockedDay (batch 84 integration): the run dates each
// take-over the day lockStock locked its company's material_document
// range for, so a run that locks on the last day of a fiscal year and
// posts after the company's midnight draws from the range it holds; a
// company whose lock was refused (it has no range) gets no day, and its
// take-over is refused for it as before.
func TestTakeOverOnTheLockedDay(t *testing.T) {
	company, m, p1, p2 := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	x := stockExtract(map[string]string{"P1/S1": "60", "P1/S2": "40", "P2/S3": "0", "NL": "0"}, moving("P1", "100", "250.01"))
	loaded := refs{"MARA/M-1": m, "T001W/P1": p1, "T001W/P2": p2, "T001L/P1/S1": uuid.New(), "T001L/P1/S2": uuid.New()}
	lastDay := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	for name, c := range map[string]struct {
		stock *lockedStock
		want  time.Time
	}{
		"locked":  {&lockedStock{day: lastDay}, lastDay},
		"refused": {&lockedStock{refused: problem.Invalid("config.no_number_range", "", "no range")}, time.Time{}},
	} {
		t.Run(name, func(t *testing.T) {
			tg := stockTarget()
			tg.units = map[string]string{"KG": "mass"}
			e := tg.entities["2000"]
			e.ID = company
			tg.entities["2000"] = e
			l := &loader{e: &Endpoints{Stock: c.stock}, ext: x, t: tg, refs: loaded}
			if err := l.lockStock(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprint(c.stock.locked); got != fmt.Sprint([]uuid.UUID{company}) {
				t.Errorf("locked: got %s, want company 2000 once", got)
			}
			if len(l.stocks) != 1 {
				t.Fatalf("stocks planned: got %d, want 1", len(l.stocks))
			}
			in, ok := l.takeOverInput(l.stocks[0])
			if !ok {
				t.Fatalf("the take-over's input: got none, issues %v", l.issues)
			}
			if !in.Day.Equal(c.want) {
				t.Errorf("the take-over's day: got %v, want %v", in.Day, c.want)
			}
		})
	}
}
