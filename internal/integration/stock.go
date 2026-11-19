package integration

import (
	"cmp"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// The stock's migration (S2p; ADR-0082 As built S2p): a material's
// valuation in a plant (MBEW: price control, price, price unit, valuation
// class, stock and value) becomes inventory's valuation, and its
// unrestricted stock at the plant's storage locations (MARD LABST) one
// initial entry (561) valued at SAP's price, its value on the class's
// stock account against the class's initial entry account, the
// migration clearing account (ADR-0082 §8). What the platform cannot
// take over of a material that loads refuses the run, an error: a stock
// left out would pass a reconciliation that leaves it out too. The stock
// of a material that does not load (held back, flagged for deletion) is
// a warning naming what is not taken over.

// sapStock is the valuation of a material that loads at one of its
// plants, as the load takes it over: SAP's keys, the company code, the
// valuation class, the platform's price control, the price per price
// unit, the stock (LBKUM) and its value (SALK3) in company code currency,
// and the stock at each storage location that holds some, in code order.
type sapStock struct {
	matnr, werks, bwkey, company string
	class, control               string
	price                        money.Minor
	priceUnit                    int32
	quantity                     *big.Rat
	value                        money.Minor
	locations                    []sapStockAt
}

// sapStockAt is a storage location's unrestricted stock (MARD LABST), a
// plain decimal in the base unit, and for a moving price what it is
// worth: the valuation's value shared out by quantity.
type sapStockAt struct {
	lgort, quantity string
	amount          money.Minor
}

func (s sapStock) key() string { return s.bwkey + "/" + s.matnr }

// stockPlan reads the extract's valuations against the plants and the
// materials that load.
type stockPlan struct {
	t      target
	areas  map[string][]sapPlant             // plants by valuation area (T001W-BWKEY)
	loaded map[string]map[string]sapMaterial // material that loads, by MATNR, then the plants it loads at
	mard   map[string][]sap.MARD             // by MATNR/WERKS
	valued map[string]bool                   // MATNR/WERKS with a valuation
	stocks []sapStock
	issues []Issue
}

func (sp *stockPlan) add(sev, table, key, format string, args ...any) {
	sp.issues = append(sp.issues, Issue{Severity: sev, Table: table, Key: key, Message: fmt.Sprintf(format, args...)})
}

// stocksOf are the valuations the load takes over, in material and
// valuation area order, and the issues the extract's valuations and
// stock raise (stockPlan.valuation, stockPlan.unvalued).
func stocksOf(e *sap.Extract, t target, plants []sapPlant, mats []sapMaterial) ([]sapStock, []Issue) {
	sp := &stockPlan{t: t, areas: map[string][]sapPlant{}, loaded: map[string]map[string]sapMaterial{}, mard: map[string][]sap.MARD{},
		valued: map[string]bool{}}
	for _, p := range plants {
		sp.areas[p.area] = append(sp.areas[p.area], p)
	}
	for _, m := range mats {
		sp.loaded[m.number] = map[string]sapMaterial{}
		for _, p := range m.plants {
			sp.loaded[m.number][p.werks] = m
		}
	}
	for _, d := range e.MARD {
		sp.mard[d.MATNR+"/"+d.WERKS] = append(sp.mard[d.MATNR+"/"+d.WERKS], d)
	}
	rows := slices.SortedFunc(slices.Values(e.MBEW), func(a, b sap.MBEW) int {
		return cmp.Or(cmp.Compare(a.MATNR, b.MATNR), cmp.Compare(a.BWKEY, b.BWKEY), cmp.Compare(a.BWTAR, b.BWTAR))
	})
	for i, r := range rows {
		if i > 0 && rows[i-1].MATNR == r.MATNR && rows[i-1].BWKEY == r.BWKEY && rows[i-1].BWTAR == r.BWTAR {
			sp.add(SevError, "MBEW", r.BWKEY+"/"+r.MATNR, "the valuation is listed twice")
			continue
		}
		sp.valuation(r)
	}
	sp.unvalued(e)
	return sp.stocks, sp.issues
}

// valuation plans one MBEW row: the valuation area must be one plant's;
// the stock of a material that does not load there is a warning; a
// material that loads is taken over when the platform can hold its
// valuation (stockOf), refused otherwise.
func (sp *stockPlan) valuation(r sap.MBEW) {
	key := r.BWKEY + "/" + r.MATNR
	plants := sp.areas[r.BWKEY]
	if len(plants) != 1 {
		werks := make([]string, len(plants))
		for i, p := range plants {
			werks[i] = p.werks
		}
		sp.add(SevError, "MBEW", key, "valuation area %s is the area of %d plants of the scope (%s); the platform values a material per plant "+
			"(ADR-0082 §7)", r.BWKEY, len(plants), strings.Join(werks, ", "))
		return
	}
	p := plants[0]
	sp.valued[r.MATNR+"/"+p.werks] = true
	m, ok := sp.loaded[r.MATNR][p.werks]
	if !ok {
		if q := money.ParseRat(r.LBKUM); q != nil && q.Sign() > 0 {
			sp.add(SevWarning, "MBEW", key, "the stock of %s %s (value %s, as extracted) at plant %s is not taken over: the material is not "+
				"migrated there", r.LBKUM, r.MATNR, r.SALK3, p.werks)
		}
		return
	}
	if !stocked(m.mtype) {
		if q := money.ParseRat(r.LBKUM); q == nil || q.Sign() != 0 {
			sp.add(SevError, "MBEW", key, "material %s is of a type that keeps no stock (%s) but SAP values stock of %q", r.MATNR, m.mtype, r.LBKUM)
		}
		return
	}
	s, reasons := sp.stockOf(r, p)
	if len(reasons) > 0 {
		sp.add(SevError, "MBEW", key, "the stock cannot be taken over: %s", strings.Join(reasons, "; "))
		return
	}
	sp.stocks = append(sp.stocks, s)
}

// stocked is whether a platform material type keeps stock
// (materials.Material.Stocked).
func stocked(mtype string) bool { return mtype != "non_stock" && mtype != "service" }

// stockOf reads a valuation the platform is to hold: no split valuation
// (BWTAR), a price control S or V, a valuation class of the company, a
// whole price unit from 1 to 10,000, prices and value in company code
// currency and not negative, a standard price above zero, and a stock
// that is the sum of the plant's storage locations' unrestricted stock
// (locations).
func (sp *stockPlan) stockOf(r sap.MBEW, p sapPlant) (sapStock, []string) {
	s := sapStock{matnr: r.MATNR, werks: p.werks, bwkey: r.BWKEY, company: p.company, class: r.BKLAS}
	var reasons []string
	why := func(format string, args ...any) { reasons = append(reasons, fmt.Sprintf(format, args...)) }
	if r.BWTAR != "" {
		why("it is split-valued (valuation type %q); the platform values a material once per plant", r.BWTAR)
	}
	switch r.VPRSV {
	case sap.PriceControlStandard:
		s.control = inventory.Standard
	case sap.PriceControlMoving:
		s.control = inventory.MovingAverage
	default:
		why("price control %q is neither S nor V", r.VPRSV)
	}
	if _, ok := sp.t.classes[p.company][r.BKLAS]; !ok {
		why("valuation class %q is no class of company %s's account determination (companies[].account_determination.inventory)", r.BKLAS, p.company)
	}
	unit, err := strconv.ParseInt(strings.TrimSpace(r.PEINH), 10, 32)
	if err != nil || unit < 1 || unit > 10_000 {
		why("price unit %q is not a whole number from 1 to 10,000", r.PEINH)
	}
	s.priceUnit = int32(unit) // #nosec G115 -- 1 to 10,000, checked above
	cur := sp.t.entities[p.company].LocalCurrency
	price := cmp.Or(map[string]string{inventory.Standard: r.STPRS, inventory.MovingAverage: r.VERPR}[s.control], "0")
	if s.price, err = sp.t.amount(price, cur); err != nil || s.price < 0 || (s.control == inventory.Standard && s.price == 0) {
		why("price %q is not an amount above zero in %s (a moving price may be zero)", price, cur)
	}
	if s.value, err = sp.t.amount(r.SALK3, cur); err != nil || s.value < 0 {
		why("value %q is not an amount of zero or more in %s", r.SALK3, cur)
	}
	reasons = append(reasons, sp.locations(&s, r)...)
	return s, reasons
}

// locations sets the stock's quantity and its storage locations' stock:
// LBKUM must be zero or a plain quantity and the sum of the plant's MARD
// LABST, none of which held at a storage location flagged for deletion,
// and a value only with stock; at a moving price each location is worth
// its share of the value by quantity (money.Allocate), so the shares add
// up to SALK3.
func (sp *stockPlan) locations(s *sapStock, r sap.MBEW) []string {
	if money.QuantityOrZero(r.LBKUM) != nil {
		return []string{fmt.Sprintf("stock %q is not a plain quantity", r.LBKUM)}
	}
	s.quantity = money.ParseRat(r.LBKUM)
	var reasons []string
	sum := new(big.Rat)
	rows := slices.SortedFunc(slices.Values(sp.mard[r.MATNR+"/"+s.werks]), func(a, b sap.MARD) int { return cmp.Compare(a.LGORT, b.LGORT) })
	for _, d := range rows {
		q := cmp.Or(strings.TrimSpace(d.LABST), "0")
		switch {
		case money.QuantityOrZero(q) != nil:
			reasons = append(reasons, fmt.Sprintf("the stock %q at storage location %s (MARD LABST) is not a plain quantity", d.LABST, d.LGORT))
		case money.ParseRat(q).Sign() == 0:
		case d.LVORM == sap.Deleted:
			reasons = append(reasons, fmt.Sprintf("storage location %s, flagged for deletion (MARD LVORM), holds stock %s", d.LGORT, q))
		default:
			sum.Add(sum, money.ParseRat(q))
			s.locations = append(s.locations, sapStockAt{lgort: d.LGORT, quantity: money.PlainQuantity(money.ParseRat(q))})
		}
	}
	switch {
	case len(reasons) > 0:
		return reasons
	case sum.Cmp(s.quantity) != 0:
		return []string{fmt.Sprintf("its stock %s (LBKUM) is not the sum of its storage locations' unrestricted stock (MARD LABST), %s: stock in "+
			"quality inspection, blocked or in transfer is not taken over", r.LBKUM, money.PlainQuantity(sum))}
	case s.quantity.Sign() == 0 && s.value != 0:
		return []string{fmt.Sprintf("it has a value of %s (SALK3) without stock", r.SALK3)}
	}
	return sp.shares(s)
}

// shares gives each storage location of a moving-priced stock its share
// of the value by quantity.
func (sp *stockPlan) shares(s *sapStock) []string {
	if s.control != inventory.MovingAverage || len(s.locations) == 0 {
		return nil
	}
	exact := make([]*big.Rat, len(s.locations))
	for i, l := range s.locations {
		exact[i] = new(big.Rat).Quo(new(big.Rat).Mul(new(big.Rat).SetInt64(int64(s.value)), money.ParseRat(l.quantity)), s.quantity)
	}
	amounts, err := money.Allocate(exact)
	if err != nil {
		return []string{fmt.Sprintf("its value cannot be shared out by quantity: %v", err)}
	}
	for i := range s.locations {
		s.locations[i].amount = amounts[i]
	}
	return nil
}

// unvalued refuses stock SAP holds of a material that loads at a plant
// without a valuation there (MARD LABST without MBEW): its value cannot
// be told.
func (sp *stockPlan) unvalued(e *sap.Extract) {
	for _, d := range e.MARD {
		_, loads := sp.loaded[d.MATNR][d.WERKS]
		q := money.ParseRat(cmp.Or(strings.TrimSpace(d.LABST), "0"))
		if loads && !sp.valued[d.MATNR+"/"+d.WERKS] && (q == nil || q.Sign() != 0) {
			sp.add(SevError, "MARD", d.MATNR+"/"+d.WERKS+"/"+d.LGORT, "the stock %q of material %s has no valuation (MBEW) at plant %s", d.LABST, d.MATNR, d.WERKS)
		}
	}
}

// stock checks the valuations and stock (S2p) after the material master,
// whose plants and materials they are of.
func (q *qualityCheck) stock() {
	plants, _ := plantsOf(q.e)                     // materialMaster reported their issues
	mats, _ := materialsOf(q.e, q.t.units, plants) // likewise
	_, issues := stocksOf(q.e, q.t, plants, mats)
	q.out = append(q.out, issues...)
}
