package replenishmentagent

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

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

// review is what one run's check knows: the run's day and plant, and
// what the run read of the proposals, their sources, their suppliers and
// the suppliers' deliveries at the plant.
type review struct {
	runDate, plant string
	proposals      map[string]proposal  // requisition id to the proposal
	sources        map[string]source    // "contract <id> item <n>", "catalog item <id>", "purchase order <id>" to the source
	suppliers      map[string]bool      // business partners read: id to whether it is an active supplier
	measures       map[string]*measures // material id to its suppliers' measures at the plant
}

// measures are what the run read of a material's suppliers' deliveries at
// the run's plant: each supplier's line naming them, and whether a read
// of every supplier was among them, so that a supplier with no line has
// no receipt rather than not having been asked about.
type measures struct {
	lines    map[string]string // supplier id to the line naming its measures
	complete bool
}

func newReview(runDate, plant string) *review {
	return &review{runDate: runDate, plant: plant, proposals: map[string]proposal{}, sources: map[string]source{}, suppliers: map[string]bool{},
		measures: map[string]*measures{}}
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

// delivery is a supplier's row of getSupplierDeliveryPerformance.
type delivery struct {
	SupplierID       string `json:"supplier_id"`
	Receipts         int    `json:"receipts"`
	DatedReceipts    int    `json:"dated_receipts"`
	OnTimeReceipts   int    `json:"on_time_receipts"`
	OnTimePercent    *int   `json:"on_time_percent"`
	PlannedLeadDays  *int   `json:"planned_lead_days"`
	ObservedLeadDays int    `json:"observed_lead_days"`
	History          string `json:"history"`
}

// minWindow is the shortest window a read measures over that the check
// takes, in days: a year, the read's default. A shorter one may hold none
// of a supplier's receipts and would let "no receipts" stand for a
// supplier that delivers.
const minWindow = 365

// performanceRead keeps the measures of a material's suppliers at the
// run's plant, as the line an order names them in. A read of another
// plant, over less than a year or ending before the run's day is no
// measure of this run's: its window may hold none of a supplier's recent
// receipts. A read
// narrowed to one supplier tells that supplier's measures, or that it has
// no receipt, and nothing of the others; a read of every supplier tells
// them all. A later read replaces what an earlier one told of the same
// supplier.
func (rv *review) performanceRead(resp apitools.Response) {
	var read struct {
		PlantID    string     `json:"plant_id"`
		MaterialID string     `json:"material_id"`
		SupplierID string     `json:"supplier_id"`
		From       string     `json:"from"`
		To         string     `json:"to"`
		Suppliers  []delivery `json:"suppliers"`
	}
	if resp.Decode(&read) != nil || read.PlantID != rv.plant || !yearLong(read.From, read.To) || read.To < rv.runDate {
		return // a refused read, another plant's, a short window's or a past one's shows nothing (ISO dates compare as text)
	}
	m := rv.measures[read.MaterialID]
	if m == nil {
		m = &measures{lines: map[string]string{}}
		rv.measures[read.MaterialID] = m
	}
	if read.SupplierID == "" {
		m.lines, m.complete = map[string]string{}, true
	} else {
		m.lines[read.SupplierID] = noReceipts
	}
	for _, d := range read.Suppliers {
		m.lines[d.SupplierID] = measured(d)
	}
}

// yearLong reports whether a read's window, both ends counted, is at
// least minWindow days.
func yearLong(from, to string) bool {
	f, err1 := time.Parse(time.DateOnly, from)
	t, err2 := time.Parse(time.DateOnly, to)
	return err1 == nil && err2 == nil && int(t.Sub(f).Hours()/24)+1 >= minWindow
}

// noReceipts is the measures line of a supplier with no receipt in the
// window.
const noReceipts = "Measures: no receipts"

// measured is the line naming a supplier's measures (S2l1): its
// receipts and their history, its observed lead time, against its
// planned one when the read gives it, and its on-time share when it gives
// one.
func measured(d delivery) string {
	m := fmt.Sprintf("Measures: %d receipts (%s), lead time %d days", d.Receipts, d.History, d.ObservedLeadDays)
	if d.PlannedLeadDays != nil {
		m += fmt.Sprintf(" against %d planned", *d.PlannedLeadDays)
	}
	if d.OnTimePercent != nil {
		m += fmt.Sprintf(", on time %d of %d (%d%%)", d.OnTimeReceipts, d.DatedReceipts, *d.OnTimePercent)
	}
	return m
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
	if err := rv.ordered(p, m[1], m[2], m[3], m[4], m[5]); err != nil {
		return err
	}
	return rv.measuresNamed(p, m[3], lines[1:])
}

// measuresNamed checks that an order names its supplier's measures as the
// run read them for the proposal's material at the run's plant (S2l1),
// "no receipts" for a supplier the read has no row of; the refusal tells
// the line wanted, which holds only what the run read.
func (rv *review) measuresNamed(p proposal, supplier string, lines []string) error {
	m := rv.measures[p.material]
	want, ok := "", false
	if m != nil {
		want, ok = m.lines[supplier]
		if !ok && m.complete {
			want, ok = noReceipts, true
		}
	}
	if !ok {
		return fmt.Errorf("%w: the run has not read getSupplierDeliveryPerformance of material %s at its plant over a year to its day, of every supplier or of %s",
			ErrMeasures, p.material, supplier)
	}
	for _, l := range lines {
		if strings.TrimSpace(l) == want {
			return nil
		}
	}
	return fmt.Errorf("%w: want %q", ErrMeasures, want)
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
