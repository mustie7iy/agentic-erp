package savingsagent

import (
	"cmp"
	"context"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
)

// opportunity is one the run found before its session: the object it
// recommends on, how a person reads it (name), why it is one, and the
// first line its recommendation starts with, with its amounts.
type opportunity struct {
	id, object, name, why string
	form                  string   // the first line's form, as the prompt gives it
	first                 string   // the first line itself
	amounts               []string // the amounts of the first line, each a tool returns as money
	currency              string   // an order's, which orders are ranked by first
}

// scan is what the run reads before its session to find the
// opportunities: the company, its legal entity and the period.
type scan struct {
	api                   *apitools.Client
	company, le, from, to string
}

// opportunities are the company's in the period: suppliers with spend off
// contract, accounts with spend from several suppliers in one currency,
// and orders priced above an active contract of their supplier; each kind
// by the currency of its first amount, in code order, and the largest
// spend first within a currency: amounts of two currencies are never
// ranked against each other. A spend query answers at most 500 rows (a
// known limit of the first version: a supplier or an account beyond them
// is not found).
func (s *scan) opportunities(ctx context.Context) ([]opportunity, error) {
	suppliers, err := s.offContract(ctx)
	if err != nil {
		return nil, err
	}
	accounts, err := s.severalSuppliers(ctx)
	if err != nil {
		return nil, err
	}
	orders, err := s.aboveContract(ctx)
	if err != nil {
		return nil, err
	}
	return slices.Concat(suppliers, accounts, orders), nil
}

// cell is a cell of a semantic query's answer.
type cell struct {
	Value    *string `json:"value"`
	Currency string  `json:"currency"`
}

// text is the cell's value, "" for none.
func (c cell) text() string {
	if c.Value == nil {
		return ""
	}
	return *c.Value
}

// query runs the spend metrics of the company in the period by the
// dimensions and returns its rows by column name, by currency and the
// largest first within one.
func (s *scan) query(ctx context.Context, measure string, by ...string) ([]map[string]cell, error) {
	body := map[string]any{"measures": []string{measure}, "by": slices.Concat(by, []string{"currency"}), "companies": []string{s.company},
		"where": []any{map[string]any{"dim": "posting_date", "op": "between", "values": []string{s.from, s.to}}}, "limit": 500}
	resp, err := s.api.Do(ctx, http.MethodPost, "/v1/analytics-queries", nil, body, "")
	var answer struct {
		Columns []struct {
			Name string `json:"name"`
		} `json:"columns"`
		Rows [][]cell `json:"rows"`
	}
	if err == nil {
		err = resp.Decode(&answer)
	}
	if err != nil {
		return nil, fmt.Errorf("savingsagent: %s of company %s by %s: %w", measure, s.company, strings.Join(by, ", "), err)
	}
	rows := make([]map[string]cell, len(answer.Rows))
	for i, r := range answer.Rows {
		rows[i] = map[string]cell{}
		for j, c := range r {
			if j < len(answer.Columns) {
				rows[i][answer.Columns[j].Name] = c
			}
		}
	}
	// Amounts of different currencies are never ranked against each
	// other: by currency, then the largest first within it.
	slices.SortStableFunc(rows, func(a, b map[string]cell) int {
		return cmp.Or(strings.Compare(a["currency"].text(), b["currency"].text()), compare(b[measure].text(), a[measure].text()))
	})
	return rows, nil
}

// rat is a decimal as the API writes it; anything else, a value of none
// among them, is zero.
func rat(d string) *big.Rat {
	r, ok := new(big.Rat).SetString(d)
	if !ok {
		return new(big.Rat)
	}
	return r
}

// positive reports whether a decimal is above zero.
func positive(d string) bool { return rat(d).Sign() > 0 }

// grouped keeps, in the order of their first row, the keys of rows and
// their rows.
type grouped struct {
	keys []string
	rows map[string][]map[string]cell
}

func (g *grouped) add(key string, row map[string]cell) {
	if g.rows == nil {
		g.rows = map[string][]map[string]cell{}
	}
	if _, ok := g.rows[key]; !ok {
		g.keys = append(g.keys, key)
	}
	g.rows[key] = append(g.rows[key], row)
}

// spendLine is a first line naming amounts per currency, in the order of
// the rows, and the amounts.
func spendLine(label, measure string, rows []map[string]cell) (string, string, []string) {
	var parts, forms, amounts []string
	for _, r := range rows {
		parts = append(parts, r[measure].text()+" "+r["currency"].text())
		forms = append(forms, "<amount> "+r["currency"].text())
		amounts = append(amounts, r[measure].text())
	}
	return label + " " + strings.Join(parts, ", "), label + " " + strings.Join(forms, ", "), amounts
}

// offContract are the suppliers with spend off contract in the period,
// each on the business partner its number names.
func (s *scan) offContract(ctx context.Context) ([]opportunity, error) {
	rows, err := s.query(ctx, "spend_off_contract", "vendor", "vendor_name")
	if err != nil {
		return nil, err
	}
	var g grouped
	for _, r := range rows {
		if positive(r["spend_off_contract"].text()) {
			g.add(r["vendor"].text(), r)
		}
	}
	var out []opportunity
	for _, number := range g.keys {
		id, err := s.lookup(ctx, "/v1/business-partners", url.Values{"number": {number}})
		if err != nil {
			return nil, fmt.Errorf("savingsagent: supplier %s: %w", number, err)
		}
		first, form, amounts := spendLine("Spend off contract", "spend_off_contract", g.rows[number])
		name := number + " " + g.rows[number][0]["vendor_name"].text()
		out = append(out, opportunity{id: id, object: SupplierObject, name: name, form: form, first: first, amounts: amounts,
			why: fmt.Sprintf("supplier %s (business partner %s) has spend off contract in the period: %s", name, id, BringUnderContract)})
	}
	return out, nil
}

// severalSuppliers are the accounts with spend from more than one
// supplier in one currency in the period, each on the account its number
// names in the company.
func (s *scan) severalSuppliers(ctx context.Context) ([]opportunity, error) {
	byVendor, err := s.query(ctx, "spend", "account", "vendor")
	if err != nil {
		return nil, err
	}
	vendors := map[string]int{} // by account and currency
	for _, r := range byVendor {
		if r["account"].text() != "" && positive(r["spend"].text()) {
			vendors[r["account"].text()+" "+r["currency"].text()]++
		}
	}
	totals, err := s.query(ctx, "spend", "account", "account_name")
	if err != nil {
		return nil, err
	}
	var g grouped
	for _, r := range totals {
		if vendors[r["account"].text()+" "+r["currency"].text()] > 1 {
			g.add(r["account"].text(), r)
		}
	}
	var out []opportunity
	for _, number := range g.keys {
		id, err := s.lookup(ctx, "/v1/accounts", url.Values{"legal_entity_id": {s.le}, "number": {number}})
		if err != nil {
			return nil, fmt.Errorf("savingsagent: account %s: %w", number, err)
		}
		first, form, amounts := spendLine("Spend", "spend", g.rows[number])
		name := number + " " + g.rows[number][0]["account_name"].text()
		out = append(out, opportunity{id: id, object: AccountObject, name: name, form: form, first: first, amounts: amounts,
			why: fmt.Sprintf("account %s (account %s) is bought from several suppliers in the period: %s", name, id, Consolidate)})
	}
	return out, nil
}

// lookup is the id of the one item a list answers with.
func (s *scan) lookup(ctx context.Context, path string, query url.Values) (string, error) {
	items, err := apitools.List[struct {
		ID string `json:"id"`
	}](ctx, s.api, path, query)
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "", fmt.Errorf("%s?%s lists none", path, query.Encode())
	}
	return items[0].ID, nil
}

// price is a price as the API writes it.
type price struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// contract is an active contract as the run reads it.
type contract struct {
	Number    string `json:"document_number"`
	Supplier  string `json:"supplier_id"`
	ValidFrom string `json:"valid_from"`
	ValidTo   string `json:"valid_to"`
	Items     []struct {
		ItemNo    int    `json:"item_no"`
		Material  string `json:"material_id"`
		UnitPrice price  `json:"unit_price"`
		PriceUnit int    `json:"price_unit"`
	} `json:"items"`
}

// order is a purchase order as the run reads it.
type order struct {
	ID        string `json:"id"`
	Number    string `json:"document_number"`
	Supplier  string `json:"supplier_id"`
	Contract  string `json:"contract_id"`
	Status    string `json:"status"`
	OrderDate string `json:"order_date"`
	Total     price  `json:"total_amount"`
	Items     []struct {
		ItemNo       int    `json:"item_no"`
		Material     string `json:"material_id"`
		UnitPrice    price  `json:"unit_price"`
		PriceUnit    int    `json:"price_unit"`
		ContractItem *int   `json:"contract_item_no"`
	} `json:"items"`
}

// aboveContract are the orders of the period, released or closed and made
// on no contract, with an item of a material priced above the item of an
// active contract of the same supplier valid on the order's date, in the
// same currency and per the same quantity; each on the order.
func (s *scan) aboveContract(ctx context.Context) ([]opportunity, error) {
	contracts, err := apitools.List[contract](ctx, s.api, "/v1/contracts", url.Values{"legal_entity_id": {s.le}, "status": {"active"}, "kind": {"contract"}})
	if err != nil {
		return nil, fmt.Errorf("savingsagent: the active contracts of company %s: %w", s.company, err)
	}
	if len(contracts) == 0 {
		return nil, nil
	}
	orders, err := apitools.List[order](ctx, s.api, "/v1/purchase-orders", url.Values{"legal_entity_id": {s.le}, "order_date_from": {s.from}, "order_date_to": {s.to}})
	if err != nil {
		return nil, fmt.Errorf("savingsagent: the purchase orders of company %s: %w", s.company, err)
	}
	var out []opportunity
	for _, o := range orders {
		if o.Contract != "" || (o.Status != "released" && o.Status != "closed") {
			continue
		}
		if c, item, ok := cheaper(o, contracts); ok {
			out = append(out, opportunity{id: o.ID, object: OrderObject, name: o.Number, form: "Ordered <amount> " + o.Total.Currency,
				first: "Ordered " + o.Total.Amount + " " + o.Total.Currency, amounts: []string{o.Total.Amount}, currency: o.Total.Currency,
				why: fmt.Sprintf("order %s (purchase order %s) has item %d above item %d of contract %s: %s", o.Number, o.ID, item, c.itemNo, c.number, UseContractPrice)})
		}
	}
	slices.SortStableFunc(out, func(a, b opportunity) int {
		return cmp.Or(strings.Compare(a.currency, b.currency), compare(b.amounts[0], a.amounts[0]))
	})
	return out, nil
}

// compare orders two decimals.
func compare(a, b string) int { return rat(a).Cmp(rat(b)) }

// cheaperItem names the contract item an order's item is priced above.
type cheaperItem struct {
	number string
	itemNo int
}

// cheaper finds the first item of the order priced above an active
// contract's item of its supplier for the same material, valid on the
// order's date, in the same currency per the same quantity; an item
// priced from a contract is not compared.
func cheaper(o order, contracts []contract) (cheaperItem, int, bool) {
	for _, it := range o.Items {
		if it.Material == "" || it.ContractItem != nil {
			continue
		}
		for _, c := range contracts {
			if c.Supplier != o.Supplier || o.OrderDate < c.ValidFrom || (c.ValidTo != "" && o.OrderDate > c.ValidTo) {
				continue
			}
			for _, ci := range c.Items {
				if ci.Material == it.Material && ci.UnitPrice.Currency == it.UnitPrice.Currency && max(ci.PriceUnit, 1) == max(it.PriceUnit, 1) &&
					compare(it.UnitPrice.Amount, ci.UnitPrice.Amount) > 0 {
					return cheaperItem{number: c.Number, itemNo: ci.ItemNo}, it.ItemNo, true
				}
			}
		}
	}
	return cheaperItem{}, 0, false
}
