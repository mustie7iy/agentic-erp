package integration

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// stockStates are the stock's two sides (S2p): SAP's valuations of the
// materials that load, planned from the extract as the load plans them
// (stocksOf), and what runs took over, read back once by SAP key, with
// the journal entries of the initial entries they posted.
type stockStates struct {
	stocks  []sapStock
	held    map[string]inventory.HeldStock // by BWKEY/MATNR
	entries []uuid.UUID
}

// readStock plans the SAP side, refusing what the quality gate refuses
// (a reconciliation run extracts again without it), and reads what runs
// of the system took over.
func (rc *reconciler) readStock(ctx context.Context) (*stockStates, error) {
	if rc.stock != nil {
		return rc.stock, nil
	}
	st, err := rc.readMaterials(ctx)
	if err != nil {
		return nil, err
	}
	out := &stockStates{held: map[string]inventory.HeldStock{}}
	var issues []Issue
	out.stocks, issues = stocksOf(rc.ext, rc.t, st.plants, st.materials)
	for _, i := range issues {
		if i.Severity == SevError {
			rc.rec.Issues = append(rc.rec.Issues, i)
		}
	}
	keyOf := map[uuid.UUID]string{}
	var valuations, documents []uuid.UUID
	for _, ref := range sortedKeys(rc.r) {
		object, key, _ := strings.Cut(ref, "/")
		switch object {
		case "MBEW":
			keyOf[rc.r[ref]], valuations = key, append(valuations, rc.r[ref])
		case "MKPF":
			documents = append(documents, rc.r[ref])
		}
	}
	held, entries, err := rc.e.Stock.TakenOver(ctx, valuations, documents)
	if err != nil {
		return nil, err
	}
	for id, h := range held {
		out.held[keyOf[id]] = h
	}
	for _, id := range entries {
		out.entries = append(out.entries, id)
	}
	slices.SortFunc(out.entries, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	rc.stock = out
	return out, nil
}

// withoutTakeOver takes the stock take-over's initial entries off the
// platform's totals: SAP's journal has no document of them, and the
// stock controls reconcile them (stockControls). What is left is what the
// run loaded of SAP's journal.
func (rc *reconciler) withoutTakeOver(ctx context.Context, s companyScope, totals ledger.Totals) (ledger.Totals, error) {
	st, err := rc.readStock(ctx)
	if err != nil || len(st.entries) == 0 {
		return totals, err
	}
	off, err := rc.e.Reports.EntryTotals(ctx, s.le, int16(rc.t.year), st.entries)
	if err != nil {
		return totals, err
	}
	for _, a := range sortedKeys(off.ByAccount) {
		if totals.ByAccount[a], err = money.Diff(totals.ByAccount[a], off.ByAccount[a]); err != nil {
			return totals, fmt.Errorf("integration: the trial balance of account %s without the stock take-over: %w", a, err)
		}
	}
	if totals.Debits, err = money.Diff(totals.Debits, off.Debits); err != nil {
		return totals, fmt.Errorf("integration: the debits without the stock take-over: %w", err)
	}
	return totals, nil
}

// stockControls compares the company's stock (S2p) where either side has
// any: each material's valuation at a plant by its SAP key, its stock at
// each storage location, its value and valuation class ("Stock"), the
// total value, and each stock account the valuations' classes post to,
// SAP's values on it against the account's balance on the platform, so a
// stock account that holds SAP's G/L balance beside the take-over shows
// twice its value.
func (rc *reconciler) stockControls(ctx context.Context, s companyScope) error {
	st, err := rc.readStock(ctx)
	if err != nil {
		return err
	}
	ms, err := rc.readMaterials(ctx)
	if err != nil {
		return err
	}
	sapSide, platform := map[string]string{}, map[string]string{}
	var sapValues, platformValues []money.Minor
	accounts := map[string][]money.Minor{}
	for _, v := range st.stocks {
		if v.company != s.cc {
			continue
		}
		sapSide[v.key()] = sapStockText(v, s.format)
		sapValues = append(sapValues, v.value)
		account := rc.t.classes[s.cc][v.class].Stock
		accounts[account] = append(accounts[account], v.value)
	}
	for key, h := range st.held {
		if h.Valuation.LegalEntityID != s.le {
			continue
		}
		platform[key] = heldStockText(h, ms, s.format)
		platformValues = append(platformValues, h.Valuation.Value)
		if _, ok := accounts[rc.t.classes[s.cc][h.Valuation.ValuationClass].Stock]; !ok {
			accounts[rc.t.classes[s.cc][h.Valuation.ValuationClass].Stock] = nil
		}
	}
	if len(sapSide) == 0 && len(platform) == 0 {
		return nil
	}
	rc.compareKeyed(s.cc, "Stock", "valuations", "stock ", sapSide, platform)
	sapTotal := rc.total("MBEW", s.cc, "the stock values of company "+s.cc, sapValues)
	platformTotal := rc.total("MBEW", s.cc, "the platform's stock values of company "+s.cc, platformValues)
	rc.add(s.cc, "Stock value total", s.format(sapTotal), s.format(platformTotal), sapTotal == platformTotal)
	return rc.stockAccounts(ctx, s, rc.totals("MBEW", s.cc, accounts, func(a string) string { return "the stock values on account " + a }))
}

// stockAccounts compares SAP's stock values per stock account with the
// account's balance on the platform in the run's fiscal year.
func (rc *reconciler) stockAccounts(ctx context.Context, s companyScope, sapAccounts map[string]money.Minor) error {
	totals, err := rc.e.Reports.Totals(ctx, s.le, int16(rc.t.year))
	if err != nil {
		return err
	}
	platform := map[string]money.Minor{}
	for a := range sapAccounts {
		platform[a] = totals.ByAccount[a]
	}
	d := diffs(s.cc, sapAccounts, platform, s.format)
	for i := range d {
		d[i].Key = "stock account " + d[i].Key
	}
	rc.rec.Diffs = append(rc.rec.Diffs, d...)
	rc.add(s.cc, "Stock accounts: accounts with differences", "0", strconv.Itoa(len(d)), len(d) == 0)
	return nil
}

// sapStockText is a valuation as a difference shows it: its class, price
// control, stock, value and stock at each storage location.
func sapStockText(v sapStock, format func(money.Minor) string) string {
	at := make([]string, len(v.locations))
	for i, l := range v.locations {
		at[i] = l.lgort + " " + l.quantity
	}
	return stockText(v.class, v.control, money.PlainQuantity(v.quantity), format(v.value), at)
}

// heldStockText is a valuation taken over as sapStockText shows SAP's, its
// storage locations by code, those holding no stock left out.
func heldStockText(h inventory.HeldStock, ms *materialStates, format func(money.Minor) string) string {
	var at []string
	for id, q := range h.Locations {
		if money.ParseRat(q).Sign() == 0 {
			continue
		}
		code := id.String()
		if u, ok := ms.units[id]; ok {
			code = u.Code
		}
		at = append(at, code+" "+q)
	}
	slices.Sort(at)
	return stockText(h.Valuation.ValuationClass, h.Valuation.PriceControl, money.PlainQuantity(money.ParseRat(h.Valuation.Quantity)),
		format(h.Valuation.Value), at)
}

func stockText(class, control, quantity, value string, at []string) string {
	return fmt.Sprintf("class %s, %s, stock %s worth %s (%s)", class, control, quantity, value, strings.Join(at, ", "))
}
