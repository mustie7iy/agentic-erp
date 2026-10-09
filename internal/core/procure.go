package core

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// CreatePurchaseOrder turns an approved requisition into a purchase order.
// When the requisition references a contract, contract prices apply and the
// contract's validity and remaining target value are enforced.
func (s *Store) CreatePurchaseOrder(reqID, vendorID, orderDate, createdBy string) (*PurchaseOrder, error) {
	req := s.Requisitions[reqID]
	if req == nil {
		return nil, fmt.Errorf("requisition %s not found", reqID)
	}
	if req.Status != ReqApproved {
		return nil, fmt.Errorf("requisition %s is %s; only approved requisitions can be ordered", reqID, req.Status)
	}
	if vendorID == "" {
		vendorID = req.VendorID
	}
	vendor := s.Vendors[vendorID]
	if vendor == nil {
		return nil, fmt.Errorf("vendor %q not found", vendorID)
	}
	if vendor.Blocked {
		return nil, fmt.Errorf("vendor %s is blocked", vendorID)
	}
	if vendor.APAccount[req.CompanyCode] == "" {
		return nil, fmt.Errorf("vendor %s is not set up for company %s", vendorID, req.CompanyCode)
	}
	var contract *Contract
	if req.ContractID != "" {
		contract = s.Contracts[req.ContractID]
		if contract == nil {
			return nil, fmt.Errorf("contract %s not found", req.ContractID)
		}
		if contract.VendorID != vendorID {
			return nil, fmt.Errorf("contract %s is with vendor %s, not %s", contract.ID, contract.VendorID, vendorID)
		}
		if orderDate < contract.ValidFrom || orderDate > contract.ValidTo {
			return nil, fmt.Errorf("contract %s is valid %s to %s", contract.ID, contract.ValidFrom, contract.ValidTo)
		}
	}
	po := &PurchaseOrder{
		ID: s.NextID("PO"), CompanyCode: req.CompanyCode, VendorID: vendorID, Currency: req.Currency,
		OrderDate: orderDate, RequisitionID: req.ID, ContractID: req.ContractID, CreatedBy: createdBy,
	}
	var value money.Minor
	for _, l := range req.Lines {
		line := POLine{LineNo: l.LineNo * 10, Material: l.Material, Description: l.Description, Quantity: l.Quantity,
			UnitPrice: l.UnitPrice, PriceUnit: 1, GLAccount: l.GLAccount}
		if contract != nil {
			i := slices.IndexFunc(contract.Items, func(it ContractItem) bool { return it.Material == l.Material })
			if i < 0 {
				return nil, fmt.Errorf("material %q is not on contract %s", l.Material, contract.ID)
			}
			it := contract.Items[i]
			line.UnitPrice, line.PriceUnit, line.GLAccount = it.UnitPrice, it.PriceUnit, it.GLAccount
		}
		if s.Account(po.CompanyCode, line.GLAccount) == nil {
			return nil, fmt.Errorf("line %d: G/L account %s does not exist in company %s", l.LineNo, line.GLAccount, po.CompanyCode)
		}
		value += money.Extend(line.Quantity, line.UnitPrice, line.PriceUnit)
		po.Lines = append(po.Lines, line)
	}
	if contract != nil {
		if contract.ReleasedValue+value > contract.TargetValue {
			return nil, fmt.Errorf("contract %s has %s left of its target value; this order needs %s",
				contract.ID, contract.TargetValue-contract.ReleasedValue, value)
		}
		contract.ReleasedValue += value
	}
	s.PurchaseOrders[po.ID] = po
	req.Status, req.POID = ReqOrdered, po.ID
	return po, nil
}

// OpenQuantity is ordered minus received for a PO line.
func (s *Store) OpenQuantity(poID string, lineNo int) (ordered, received money.Qty, err error) {
	po := s.PurchaseOrders[poID]
	if po == nil {
		return 0, 0, fmt.Errorf("purchase order %s not found", poID)
	}
	pl := po.line(lineNo)
	if pl == nil {
		return 0, 0, fmt.Errorf("purchase order %s has no line %d", poID, lineNo)
	}
	for _, g := range s.ReceiptsFor(poID) {
		if g.POLineNo == lineNo {
			received += g.Quantity
		}
	}
	return pl.Quantity, received, nil
}

// PostGoodsReceipt records a receipt (or, with reverse, a reversal) against
// a PO line. Receipts may exceed the ordered quantity by at most
// overDeliveryPercent; reversals may not exceed what was received.
func (s *Store) PostGoodsReceipt(poID string, lineNo int, qty money.Qty, reverse bool, overDeliveryPercent int, date, by string) (*GoodsReceipt, error) {
	if qty <= 0 {
		return nil, errors.New("quantity must be positive")
	}
	ordered, received, err := s.OpenQuantity(poID, lineNo)
	if err != nil {
		return nil, err
	}
	gr := &GoodsReceipt{ID: s.NextID("GR"), POID: poID, POLineNo: lineNo, Quantity: qty, PostingDate: date, MovementType: "101", PostedBy: by}
	if reverse {
		if qty > received {
			return nil, fmt.Errorf("cannot reverse %s; only %s received", qty, received)
		}
		gr.Quantity, gr.MovementType = -qty, "102"
	} else {
		allowed := ordered + money.Qty(int64(ordered)*int64(overDeliveryPercent)/100)
		if received+qty > allowed {
			return nil, fmt.Errorf("receipt would bring line %d to %s; ordered %s, over-delivery tolerance %d%%", lineNo, received+qty, ordered, overDeliveryPercent)
		}
	}
	s.GoodsReceipts[gr.ID] = gr
	return gr, nil
}

// VendorWarnings flags likely duplicates of a proposed vendor.
func (s *Store) VendorWarnings(v *Vendor) []string {
	var out []string
	name := normalizeName(v.Name)
	for _, id := range SortedKeys(s.Vendors) {
		other := s.Vendors[id]
		if other.ID == v.ID {
			continue
		}
		if v.TaxID != "" && other.TaxID == v.TaxID {
			out = append(out, fmt.Sprintf("vendor %s already has tax number %s", other.ID, v.TaxID))
		} else if normalizeName(other.Name) == name {
			out = append(out, fmt.Sprintf("vendor %s has the same name", other.ID))
		}
	}
	if v.TaxID == "" {
		out = append(out, "no tax number")
	}
	return out
}

func normalizeName(n string) string { return strings.ToUpper(strings.Join(strings.Fields(n), " ")) }

// ValidateNewVendor checks a proposed vendor can be created.
func (s *Store) ValidateNewVendor(v *Vendor) error {
	switch {
	case v.ID == "":
		return errors.New("vendor id is required")
	case s.Vendors[v.ID] != nil:
		return fmt.Errorf("vendor %s already exists", v.ID)
	case strings.TrimSpace(v.Name) == "":
		return errors.New("vendor name is required")
	case len(v.APAccount) == 0:
		return errors.New("vendor needs an AP reconciliation account in at least one company")
	}
	for cc, acct := range v.APAccount {
		a := s.Account(cc, acct)
		if a == nil || !a.VendorRecon {
			return fmt.Errorf("account %s in company %s is not a vendor reconciliation account", acct, cc)
		}
	}
	return nil
}

// ProposePayments selects every open vendor item due by runDate and groups
// it by vendor. Vendors that are blocked, have no bank account, have a bank
// change pending, or changed bank details within holdDays are excluded with
// a reason, as are vendors whose open items net to a debit.
func (s *Store) ProposePayments(company, runDate string, holdDays int, proposedBy string) (*PaymentRun, error) {
	if s.Companies[company] == nil {
		return nil, fmt.Errorf("company %s not found", company)
	}
	run, err := time.Parse(time.DateOnly, runDate)
	if err != nil {
		return nil, fmt.Errorf("run date: %w", err)
	}
	pendingBank := map[string]bool{}
	for _, c := range s.MDChanges {
		if c.Kind == VendorBankChange && c.Status == ChangePending {
			pendingBank[c.VendorID] = true
		}
	}
	reserved := s.reservedItems()
	items := s.openVendorItems(company)
	pr := &PaymentRun{ID: s.NextID("PAY"), CompanyCode: company, RunDate: runDate, ProposedBy: proposedBy, Status: RunProposed}
	for _, vendorID := range SortedKeys(items) {
		v := s.Vendors[vendorID]
		vp := VendorPayment{VendorID: vendorID, VendorName: v.Name, BankAccount: v.BankAccount}
		for _, it := range items[vendorID] {
			if reserved[itemKey(it.JournalEntryID, it.Line)] {
				continue
			}
			due, err := dueDate(it.postingDate, v.PaymentTermsDays)
			if err != nil {
				return nil, err
			}
			if due.After(run) {
				continue
			}
			vp.Items = append(vp.Items, PaidItem{JournalEntryID: it.JournalEntryID, Line: it.Line, Reference: it.Reference,
				DueDate: due.Format(time.DateOnly), Amount: -it.Amount})
			vp.Total -= it.Amount
		}
		if len(vp.Items) == 0 {
			continue
		}
		reason := ""
		switch {
		case v.Blocked:
			reason = "vendor is blocked for posting"
		case v.BankAccount == "":
			reason = "vendor has no bank account"
		case pendingBank[vendorID]:
			reason = "bank change awaiting approval"
		case recentBankChange(v.BankChangedAt, run, holdDays):
			reason = fmt.Sprintf("bank details changed on %s; held for %d days", v.BankChangedAt, holdDays)
		case vp.Total <= 0:
			reason = "open items net to a debit balance"
		}
		if reason != "" {
			pr.Excluded = append(pr.Excluded, Exclusion{VendorID: vendorID, Reason: reason, Amount: vp.Total})
			continue
		}
		pr.Payments = append(pr.Payments, vp)
		pr.Total += vp.Total
	}
	s.PaymentRuns[pr.ID] = pr
	return pr, nil
}

type vendorItem struct {
	JournalEntryID string
	Line           int
	Reference      string
	postingDate    string
	Amount         money.Minor
}

// openVendorItems collects uncleared items on vendor reconciliation
// accounts in one pass over the journal, grouped by vendor.
func (s *Store) openVendorItems(company string) map[string][]vendorItem {
	out := map[string][]vendorItem{}
	for _, id := range SortedKeys(s.Journal) {
		je := s.Journal[id]
		if je.CompanyCode != company {
			continue
		}
		for i, l := range je.Lines {
			if l.VendorID == "" || l.ClearedBy != "" {
				continue
			}
			if a := s.Account(company, l.Account); a == nil || !a.VendorRecon {
				continue
			}
			out[l.VendorID] = append(out[l.VendorID], vendorItem{JournalEntryID: je.ID, Line: i, Reference: je.Reference, postingDate: je.PostingDate, Amount: l.Amount})
		}
	}
	return out
}

// reservedItems are open items already in a proposed (unreleased) run, so
// two runs never pay the same invoice.
func (s *Store) reservedItems() map[string]bool {
	out := map[string]bool{}
	for _, r := range s.PaymentRuns {
		if r.Status != RunProposed {
			continue
		}
		for _, p := range r.Payments {
			for _, it := range p.Items {
				out[itemKey(it.JournalEntryID, it.Line)] = true
			}
		}
	}
	return out
}

func itemKey(je string, line int) string { return fmt.Sprintf("%s#%d", je, line) }

func dueDate(posting string, days int) (time.Time, error) {
	t, err := time.Parse(time.DateOnly, posting)
	if err != nil {
		return time.Time{}, fmt.Errorf("posting date %q: %w", posting, err)
	}
	return t.AddDate(0, 0, days), nil
}

func recentBankChange(changed string, run time.Time, holdDays int) bool {
	if changed == "" {
		return false
	}
	t, err := time.Parse(time.DateOnly, changed)
	if err != nil {
		return true // unknown date: hold to be safe
	}
	return run.Before(t.AddDate(0, 0, holdDays))
}

// ExecutePaymentRun posts one payment per vendor (Dr AP, Cr bank) and
// clears the paid items. Every item is re-checked first: if anything was
// cleared or changed since the proposal, nothing is posted.
func (s *Store) ExecutePaymentRun(runID, bankAccount, postingDate string) ([]*JournalEntry, error) {
	pr := s.PaymentRuns[runID]
	if pr == nil {
		return nil, fmt.Errorf("payment run %s not found", runID)
	}
	if pr.Status != RunProposed {
		return nil, fmt.Errorf("payment run %s is %s", runID, pr.Status)
	}
	if a := s.Account(pr.CompanyCode, bankAccount); a == nil || a.Type != Asset {
		return nil, fmt.Errorf("bank account %s is not an asset account in company %s", bankAccount, pr.CompanyCode)
	}
	for _, p := range pr.Payments {
		for _, it := range p.Items {
			je := s.Journal[it.JournalEntryID]
			if je == nil || it.Line >= len(je.Lines) {
				return nil, fmt.Errorf("item %s line %d no longer exists", it.JournalEntryID, it.Line)
			}
			l := je.Lines[it.Line]
			if l.ClearedBy != "" || l.VendorID != p.VendorID || -l.Amount != it.Amount {
				return nil, fmt.Errorf("item %s line %d changed since the proposal; propose the run again", it.JournalEntryID, it.Line)
			}
		}
	}
	// Build and validate every entry before posting any, so a run is posted
	// completely or not at all.
	entries := make([]*JournalEntry, 0, len(pr.Payments))
	for _, p := range pr.Payments {
		apAccount := s.Vendors[p.VendorID].APAccount[pr.CompanyCode]
		je := &JournalEntry{
			ID: s.NextID("JE"), CompanyCode: pr.CompanyCode, PostingDate: postingDate, DocumentType: "ZP",
			Reference: pr.ID, Source: "native:payment:" + pr.ID,
			Lines: []JournalLine{
				{Account: apAccount, Amount: p.Total, VendorID: p.VendorID, Text: "Payment " + pr.ID},
				{Account: bankAccount, Amount: -p.Total, Text: "Payment to " + p.VendorName},
			},
		}
		if err := s.ValidateEntry(je); err != nil {
			return nil, err
		}
		entries = append(entries, je)
	}
	for i, je := range entries {
		je.Lines[0].ClearedBy = je.ID
		s.Journal[je.ID] = je
		for _, it := range pr.Payments[i].Items {
			s.Journal[it.JournalEntryID].Lines[it.Line].ClearedBy = je.ID
		}
		pr.JournalEntryIDs = append(pr.JournalEntryIDs, je.ID)
	}
	out := entries
	pr.Status, pr.ReleasedAt = RunReleased, postingDate
	return out, nil
}
