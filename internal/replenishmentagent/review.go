package replenishmentagent

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

// proposal is a requisition as the run read it.
type proposal struct {
	legalEntity, status, neededBy string
	material, quantity, unit      string
	items                         int
}

// source is a source of supply of a material the run read: its company,
// material and supplier and the days it is valid on ("" for no bound).
type source struct {
	legalEntity, material, supplier string
	from, to                        string
}

// review is what one run's check knows: the run's day, and what the run
// read of the proposals, their sources and their suppliers.
type review struct {
	runDate   string
	proposals map[string]proposal // requisition id to the proposal
	sources   map[string]source   // "contract <id> item <n>", "catalog item <id>", "purchase order <id>" to the source
	suppliers map[string]bool     // business partners read: id to whether it is an active supplier
}

func newReview(runDate string) *review {
	return &review{runDate: runDate, proposals: map[string]proposal{}, sources: map[string]source{}, suppliers: map[string]bool{}}
}

// quantity is a Quantity as an answer gives it.
type quantity struct {
	Value string `json:"value"`
	Unit  string `json:"unit"`
}

// requisitionRead keeps a requisition the run read.
func (rv *review) requisitionRead(resp apitools.Response) {
	var r struct {
		ID            string `json:"id"`
		LegalEntityID string `json:"legal_entity_id"`
		Status        string `json:"status"`
		NeededBy      string `json:"needed_by"`
		Items         []struct {
			MaterialID string   `json:"material_id"`
			Quantity   quantity `json:"quantity"`
		} `json:"items"`
	}
	if resp.Decode(&r) != nil {
		return // a refused read shows no requisition
	}
	p := proposal{legalEntity: r.LegalEntityID, status: r.Status, neededBy: r.NeededBy, items: len(r.Items)}
	if len(r.Items) > 0 {
		p.material, p.quantity, p.unit = r.Items[0].MaterialID, r.Items[0].Quantity.Value, r.Items[0].Quantity.Unit
	}
	rv.proposals[r.ID] = p
}

// catalogRead keeps the active catalog items of a material in an answer.
func (rv *review) catalogRead(resp apitools.Response) {
	var page struct {
		Items []struct {
			ID            string `json:"id"`
			LegalEntityID string `json:"legal_entity_id"`
			SupplierID    string `json:"supplier_id"`
			MaterialID    string `json:"material_id"`
			Status        string `json:"status"`
		}
	}
	if resp.Decode(&page) != nil {
		return // a refused read shows nothing
	}
	for _, c := range page.Items {
		if c.Status == "active" && c.MaterialID != "" {
			rv.sources["catalog item "+c.ID] = source{legalEntity: c.LegalEntityID, material: c.MaterialID, supplier: c.SupplierID}
		}
	}
}

// contractsRead keeps the items of a material of the active contracts in
// an answer, each valid from and to the contract's days.
func (rv *review) contractsRead(resp apitools.Response) {
	var page struct {
		Items []struct {
			ID            string `json:"id"`
			LegalEntityID string `json:"legal_entity_id"`
			SupplierID    string `json:"supplier_id"`
			Status        string `json:"status"`
			ValidFrom     string `json:"valid_from"`
			ValidTo       string `json:"valid_to"`
			Items         []struct {
				ItemNo     int    `json:"item_no"`
				MaterialID string `json:"material_id"`
			} `json:"items"`
		}
	}
	if resp.Decode(&page) != nil {
		return // a refused read shows nothing
	}
	for _, c := range page.Items {
		for _, it := range c.Items {
			if c.Status == "active" && it.MaterialID != "" {
				rv.sources[fmt.Sprintf("contract %s item %d", c.ID, it.ItemNo)] = source{legalEntity: c.LegalEntityID, material: it.MaterialID,
					supplier: c.SupplierID, from: c.ValidFrom, to: c.ValidTo}
			}
		}
	}
}

// ordersRead keeps the materials of the released and closed purchase
// orders in an answer: what was ordered, and from whom.
func (rv *review) ordersRead(resp apitools.Response) {
	var page struct {
		Items []struct {
			ID            string `json:"id"`
			LegalEntityID string `json:"legal_entity_id"`
			SupplierID    string `json:"supplier_id"`
			Status        string `json:"status"`
			Items         []struct {
				MaterialID string `json:"material_id"`
			} `json:"items"`
		}
	}
	if resp.Decode(&page) != nil {
		return // a refused read shows nothing
	}
	for _, o := range page.Items {
		if o.Status != "released" && o.Status != "closed" {
			continue
		}
		for _, it := range o.Items {
			if it.MaterialID != "" {
				// An order of several items of the material is one source of it.
				rv.sources["purchase order "+o.ID+" "+it.MaterialID] = source{legalEntity: o.LegalEntityID, material: it.MaterialID, supplier: o.SupplierID}
			}
		}
	}
}

// supplierRead keeps whether a business partner read is an active
// supplier.
func (rv *review) supplierRead(resp apitools.Response) {
	var bp struct {
		ID     string   `json:"id"`
		Status string   `json:"status"`
		Roles  []string `json:"roles"`
	}
	if resp.Decode(&bp) != nil {
		return // a refused read shows no supplier
	}
	rv.suppliers[bp.ID] = bp.Status == "active" && slices.Contains(bp.Roles, "supplier")
}

// open are the statuses of a requisition still to be ordered.
var open = []string{"draft", "pending_approval", "approved"}

// orderLine is an order's first line: the quantity and unit, the
// supplier, the day and the source.
var orderLine = regexp.MustCompile(`^Order (\d+(?:\.\d+)?) ([A-Z0-9]{1,6}) from supplier ([0-9a-f-]{36}) by (\d{4}-\d{2}-\d{2}): ` +
	`(contract [0-9a-f-]{36} item \d+|catalog item [0-9a-f-]{36}|purchase order [0-9a-f-]{36})$`)

// check is the agent's rule on a recommendation the pattern admits
// (ADR-0072, S2l): on a proposal the run read that is still to be ordered,
// of one material; every number it writes but an order's first line,
// which is checked whole, is one the run read; an order in the form a
// buyer orders from, of the proposal's quantity by its day from a source
// of its material the run read and that source's active supplier; a
// source_needed only when the run read no source of the material.
func (rv *review) check(r recommend.Proposed, f *recommend.Figures) error {
	p, read := rv.proposals[r.ObjectID]
	if !read || !slices.Contains(open, p.status) || p.items != 1 || p.material == "" {
		return fmt.Errorf("%w: not %s", ErrProposal, r.ObjectID)
	}
	lines := strings.Split(r.Content, "\n")
	first := strings.TrimSpace(lines[0])
	if r.Kind == SourceNeeded {
		return rv.sourceNeeded(p, r.Content, first, f)
	}
	if unread := f.Unread(joined(lines, 0)); len(unread) > 0 {
		return fmt.Errorf("%w: not %s", ErrUnread, strings.Join(unread, ", "))
	}
	m := orderLine.FindStringSubmatch(first)
	if m == nil || !reasoned(lines[1:]) {
		return fmt.Errorf("%w: not %q", ErrForm, first)
	}
	return rv.ordered(p, m[1], m[2], m[3], m[4], m[5])
}

// sourceNeeded checks a source_needed: it orders nothing, writes only
// numbers the run read, and the run read no source an order of the
// proposal could be made from (usable) whose supplier it has not read as
// inactive. The refusal names the first such source in the order of its
// key, so a run is told the same one each time.
func (rv *review) sourceNeeded(p proposal, content, first string, f *recommend.Figures) error {
	if strings.HasPrefix(first, "Order ") {
		return fmt.Errorf("%w: not %q", ErrForm, first)
	}
	if unread := f.Unread(content); len(unread) > 0 {
		return fmt.Errorf("%w: not %s", ErrUnread, strings.Join(unread, ", "))
	}
	day := rv.day(p)
	for _, ref := range slices.Sorted(maps.Keys(rv.sources)) {
		s := rv.sources[ref]
		if active, read := rv.suppliers[s.supplier]; usable(s, p, day) && (active || !read) {
			return fmt.Errorf("%w: %s", ErrHasSource, strings.TrimSuffix(ref, " "+s.material))
		}
	}
	return nil
}

// day is the day an order of the proposal is by: the day it is needed by,
// or the run's day when it names none.
func (rv *review) day(p proposal) string {
	if p.neededBy == "" {
		return rv.runDate
	}
	return p.neededBy
}

// usable reports whether an order of the proposal by the day can be made
// from the source: one of the proposal's material in its company, valid
// on the day. order and source_needed hold to the same test.
func usable(s source, p proposal, day string) bool {
	return s.legalEntity == p.legalEntity && s.material == p.material && (s.from == "" || day >= s.from) && (s.to == "" || day <= s.to)
}

// ordered checks an order's quantity, day, source and supplier against
// the proposal.
func (rv *review) ordered(p proposal, qty, unit, supplier, by, ref string) error {
	day := rv.day(p)
	s, known := rv.sources[ref]
	if strings.HasPrefix(ref, "purchase order ") {
		s, known = rv.sources[ref+" "+p.material]
	}
	switch {
	case qty != p.quantity || unit != p.unit:
		return fmt.Errorf("%w: not %s %s", ErrQuantity, qty, unit)
	case by != day:
		return fmt.Errorf("%w: not %s", ErrWhen, by)
	case !known || !usable(s, p, day) || s.supplier != supplier:
		return fmt.Errorf("%w: not %s from %s", ErrSource, ref, supplier)
	case !rv.suppliers[supplier]:
		return fmt.Errorf("%w: not %s", ErrSupplier, supplier)
	}
	return nil
}

// reasoned reports whether the lines after an order's first give its
// reasons.
func reasoned(lines []string) bool {
	for _, l := range lines {
		if why, ok := strings.CutPrefix(strings.TrimSpace(l), "Why: "); ok && strings.TrimSpace(why) != "" {
			return true
		}
	}
	return false
}
