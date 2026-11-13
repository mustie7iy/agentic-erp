package analytics

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
)

// The spend lines (dataset spend_lines v1, T91b; ADR-0044 As built T91b):
// one fact per item of a posted supplier invoice, credit memo or
// subsequent debit or credit, dated its posting date, at the item's net
// amount in the document's currency, negative on a credit memo and a
// subsequent credit. Each names its supplier, its account and cost
// center, the item's own or, for an item on a purchase order, the order
// item's, and the contract the order was made on; an item without an
// order, or on an order without a contract, is off contract.

const spendLines = "spend_lines"

// spendInvoice is a posted supplier invoice as the API lists it.
type spendInvoice struct {
	ID          string
	SupplierID  string `json:"supplier_id"`
	PostingDate string `json:"posting_date"`
	Kind        string
	Items       []struct {
		PurchaseOrderID string `json:"purchase_order_id"`
		POItemNo        int    `json:"po_item_no"`
		Amount          amount
		AccountID       string `json:"account_id"`
		CostCenterID    string `json:"cost_center_id"`
	}
}

// spendOrder is a purchase order as the API lists it: its contract and
// what its items are assigned to.
type spendOrder struct {
	ID         string
	ContractID string `json:"contract_id"`
	Items      []struct {
		ItemNo       int    `json:"item_no"`
		AccountID    string `json:"account_id"`
		CostCenterID string `json:"cost_center_id"`
	}
}

// costCenter and contract are a cost center and a contract as the API
// lists them: the id and what names it.
type (
	costCenter struct{ ID, Code string }
	contract   struct {
		ID             string
		DocumentNumber string `json:"document_number"`
	}
)

// assignment is what an order item gives an invoice item on it: its
// account and its cost center, each "" for none.
type assignment struct{ account, costCenter string }

// spendRefs is what names a legal entity's spend beside the invoices
// themselves: its order items' assignments by order and item number, the
// number of the contract each order was made on by order ("" for none),
// and its cost centers' codes by id.
type spendRefs struct {
	onOrder   map[string]assignment
	contracts map[string]string
	centers   map[string]string
}

// credits are the kinds whose amounts reduce spend.
var credits = map[string]bool{"credit_memo": true, "subsequent_credit": true}

// addSpend adds the legal entity's posted supplier invoices' items to
// spend_lines. The API's list selects the status, so a document parked,
// on hold, rejected, cancelled or reversed is never read.
func (s Source) addSpend(ctx context.Context, b *factBuilder, le legalEntity, byLE url.Values, chart map[string]account) error {
	if !s.builds(spendLines) {
		return nil
	}
	invoices, err := apitools.List[spendInvoice](ctx, s.API, "/v1/supplier-invoices", url.Values{"legal_entity_id": byLE["legal_entity_id"], "status": {"posted"}})
	if err != nil {
		return err
	}
	refs, err := s.spendRefs(ctx, byLE)
	if err != nil {
		return err
	}
	for _, inv := range invoices {
		if err := b.addSpendInvoice(le, inv, chart, refs); err != nil {
			return err
		}
	}
	return nil
}

// spendRefs reads the legal entity's purchase orders, contracts and cost
// centers as the caller, in that order.
func (s Source) spendRefs(ctx context.Context, byLE url.Values) (spendRefs, error) {
	refs := spendRefs{onOrder: map[string]assignment{}, contracts: map[string]string{}, centers: map[string]string{}}
	orders, err := apitools.List[spendOrder](ctx, s.API, "/v1/purchase-orders", byLE)
	if err != nil {
		return refs, err
	}
	contracts, err := apitools.List[contract](ctx, s.API, "/v1/contracts", byLE)
	if err != nil {
		return refs, err
	}
	centers, err := apitools.List[costCenter](ctx, s.API, "/v1/cost-centers", byLE)
	if err != nil {
		return refs, err
	}
	numbers := map[string]string{}
	for _, c := range contracts {
		numbers[c.ID] = c.DocumentNumber
	}
	for _, c := range centers {
		refs.centers[c.ID] = c.Code
	}
	for _, o := range orders {
		refs.contracts[o.ID] = numbers[o.ContractID]
		for _, it := range o.Items {
			refs.onOrder[orderItem(o.ID, it.ItemNo)] = assignment{account: it.AccountID, costCenter: it.CostCenterID}
		}
	}
	return refs, nil
}

// orderItem keys a purchase order's item.
func orderItem(order string, item int) string { return order + "/" + strconv.Itoa(item) }

// addSpendInvoice adds one fact per item of the posted document. An item
// on an order takes the order's contract, whatever became of the order's
// item, and the order item's account and cost center where it names none
// of its own.
func (b *factBuilder) addSpendInvoice(le legalEntity, inv spendInvoice, chart map[string]account, refs spendRefs) error {
	v := b.vendors[inv.SupplierID]
	for _, it := range inv.Items {
		as, contract := assignment{account: it.AccountID, costCenter: it.CostCenterID}, ""
		if it.PurchaseOrderID != "" {
			of := refs.onOrder[orderItem(it.PurchaseOrderID, it.POItemNo)]
			as = assignment{account: cmp.Or(as.account, of.account), costCenter: cmp.Or(as.costCenter, of.costCenter)}
			contract = refs.contracts[it.PurchaseOrderID]
		}
		a := chart[as.account]
		dims := map[string]string{"company": le.Code, "currency": it.Amount.Currency, "vendor": v.Number, "vendor_name": v.Name,
			"account": a.Number, "account_name": a.Name, "cost_center": refs.centers[as.costCenter], "contract": contract,
			"on_contract": strconv.FormatBool(contract != "")}
		net, err := b.minor(it.Amount)
		if err == nil {
			err = b.calendar(dims, "posting_date", inv.PostingDate)
		}
		if err != nil {
			return fmt.Errorf("invoice %s: %w", inv.ID, err)
		}
		if credits[inv.Kind] {
			net = -net
		}
		b.f.Tables[spendLines] = append(b.f.Tables[spendLines], Fact{ID: inv.ID, Dims: dims, Amount: net, Currency: it.Amount.Currency})
	}
	return nil
}
