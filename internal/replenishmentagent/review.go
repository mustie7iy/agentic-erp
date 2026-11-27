package replenishmentagent

import (
	"cmp"
	"fmt"
	"maps"
	"math/big"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

// proposal is a requisition as the run read it: of one material in one
// unit, its quantity what its items add up to and its lots the items, one
// per lot of the plan's lot sizing (ADR-0084 As built S2k2); material is
// empty for one that is not (no item, items of several materials or
// units, or a quantity that is no plain decimal).
type proposal struct {
	legalEntity, status, neededBy string
	material, quantity, unit      string
	lots                          []lot
}

// lot is an item of a proposal: its number and quantity as the
// requisition gives them.
type lot struct {
	itemNo   int
	quantity string
}

// source is a source of supply of a material the run read: its company,
// material and supplier and the days it is valid on ("" for no bound).
type source struct {
	legalEntity, material, supplier string
	from, to                        string
}

// review is what one run's check knows: the run's plant and the year
// ending on its day, what the run read of the proposals, their sources
// and their suppliers, and the suppliers' measures the check read itself.
type review struct {
	runDate, plant string
	from           string                                      // the first day of the year ending on the run's day
	read           func(url.Values) (apitools.Response, error) // getSupplierDeliveryPerformance, as the check reads it
	proposals      map[string]proposal                         // requisition id to the proposal
	sources        map[string]source                           // "contract <id> item <n>", "catalog item <id>", "purchase order <id>" to the source
	suppliers      map[string]bool                             // business partners read: id to whether it is an active supplier
	measured       map[string]string                           // "<material> <supplier>" to the line naming the supplier's measures
}

// window is the days, both counted, a supplier's measures are read over:
// the year ending on the run's day (ADR-0072 As built S2l1).
const window = 365

// windowFrom is the first day of the year ending on a run's day, the
// check's window's from; a run day that is no date has none.
func windowFrom(runDate string) (string, error) {
	d, err := time.Parse(time.DateOnly, runDate)
	if err != nil {
		return "", fmt.Errorf("replenishmentagent: the planning run's day %q: %w", runDate, err)
	}
	return d.AddDate(0, 0, 1-window).Format(time.DateOnly), nil
}

func newReview(runDate, from, plant string, read func(url.Values) (apitools.Response, error)) *review {
	return &review{runDate: runDate, plant: plant, from: from, read: read, proposals: map[string]proposal{}, sources: map[string]source{},
		suppliers: map[string]bool{}, measured: map[string]string{}}
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
		Items         []item `json:"items"`
	}
	if resp.Decode(&r) != nil {
		return // a refused read shows no requisition
	}
	p := proposal{legalEntity: r.LegalEntityID, status: r.Status, neededBy: r.NeededBy}
	p.material, p.quantity, p.unit, p.lots = lotsOf(r.Items)
	rv.proposals[r.ID] = p
}

// item is a requisition's item as an answer gives it.
type item struct {
	ItemNo     int      `json:"item_no"`
	MaterialID string   `json:"material_id"`
	Quantity   quantity `json:"quantity"`
}

// lotsOf is the material, total quantity, unit and lots of a proposal's
// items, the lots in item order and the total written as the run line
// writes proposed (trim_scale, money.PlainQuantity); no material when
// they are not lots of one material in one unit.
func lotsOf(items []item) (material, total, unit string, lots []lot) {
	sum := new(big.Rat)
	for _, it := range items {
		q, err := money.ParsePlain(it.Quantity.Value)
		if err != nil || it.MaterialID == "" || it.MaterialID != items[0].MaterialID || it.Quantity.Unit != items[0].Quantity.Unit {
			return "", "", "", nil
		}
		sum.Add(sum, q)
		lots = append(lots, lot{itemNo: it.ItemNo, quantity: it.Quantity.Value})
	}
	if len(lots) == 0 {
		return "", "", "", nil
	}
	// In item order, whatever order an answer gives them in.
	slices.SortFunc(lots, func(a, b lot) int { return cmp.Compare(a.itemNo, b.itemNo) })
	return items[0].MaterialID, money.PlainQuantity(sum), items[0].Quantity.Unit, lots
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

// measuresOf is the line naming the supplier's measures of the material
// at the run's plant over the year ending on the run's day, as the check
// reads them itself (getSupplierDeliveryPerformance narrowed to the
// supplier, from and to fixed): no read of the model's, whatever window
// or supplier it asked for, makes an order pass or fail. A supplier
// without a row has no receipt in the year. A read is kept for the run,
// so that the check tells the same line each time; a failed one is not.
func (rv *review) measuresOf(material, supplier string) (string, error) {
	key := material + " " + supplier
	if line, ok := rv.measured[key]; ok {
		return line, nil
	}
	resp, err := rv.read(url.Values{"plant_id": {rv.plant}, "material_id": {material}, "supplier_id": {supplier}, "from": {rv.from}, "to": {rv.runDate}})
	var read struct {
		Suppliers []delivery `json:"suppliers"`
	}
	if err == nil {
		err = resp.Decode(&read)
	}
	if err != nil {
		return "", fmt.Errorf("replenishmentagent: the delivery performance of supplier %s for material %s from %s to %s: %w", supplier, material, rv.from, rv.runDate, err)
	}
	line := noReceipts
	for _, d := range read.Suppliers {
		if d.SupplierID == supplier {
			line = measured(d)
		}
	}
	rv.measured[key] = line
	return line, nil
}

// measuresPrefix begins the line naming an order's supplier's measures.
const measuresPrefix = "Measures: "

// noReceipts is the measures line of a supplier with no receipt in the
// year.
const noReceipts = measuresPrefix + "no receipts"

// measured is the line naming a supplier's measures (S2l1): its
// receipts and their history, its observed lead time, against its
// planned one when the read gives it, and its on-time share when it gives
// one.
func measured(d delivery) string {
	m := fmt.Sprintf(measuresPrefix+"%d receipts (%s), lead time %d days", d.Receipts, d.History, d.ObservedLeadDays)
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
// of one material; every number it writes but in an order's first line,
// its lots and its measures, which are checked whole, is one the run
// read; an order in the form a buyer orders from, of the proposal's
// quantity (its lots' sum) by its day from a source of its material the
// run read and that source's active supplier, naming each of its lots
// when it has several and the supplier's measures as the check reads
// them; a source_needed only when the run read no source of the
// material.
func (rv *review) check(r recommend.Proposed, f *recommend.Figures) error {
	p, read := rv.proposals[r.ObjectID]
	if !read || !slices.Contains(open, p.status) || p.material == "" {
		return fmt.Errorf("%w: not %s", ErrProposal, r.ObjectID)
	}
	lines := strings.Split(r.Content, "\n")
	first := strings.TrimSpace(lines[0])
	if r.Kind == SourceNeeded {
		return rv.sourceNeeded(p, r.Content, first, f)
	}
	if unread := f.Unread(figured(lines)); len(unread) > 0 {
		return fmt.Errorf("%w: not %s", ErrUnread, strings.Join(unread, ", "))
	}
	m := orderLine.FindStringSubmatch(first)
	if m == nil || !reasoned(lines[1:]) {
		return fmt.Errorf("%w: not %q", ErrForm, first)
	}
	if err := rv.ordered(p, m[1], m[2], m[3], m[4], m[5]); err != nil {
		return err
	}
	if err := rv.lotsNamed(p, lines[1:]); err != nil {
		return err
	}
	return rv.measuresNamed(p, m[3], lines[1:])
}

// lotPrefix begins a line naming one of an order's lots.
const lotPrefix = "Lot "

// lotsNamed checks the lines naming an order's lots (S2k2): they are the
// proposal's lots in its items' order, each "Lot <item_no>: <quantity>
// <unit> by <YYYY-MM-DD>" with the item's number and quantity as the
// requisition gives them, its unit and the order's day, so that the lots
// add up to the order's quantity. A proposal of several lots names them
// all; one of one lot may name it or none. The refusal tells the lines
// wanted.
func (rv *review) lotsNamed(p proposal, lines []string) error {
	want := make([]string, len(p.lots))
	for i, l := range p.lots {
		want[i] = fmt.Sprintf("%s%d: %s %s by %s", lotPrefix, l.itemNo, l.quantity, p.unit, rv.day(p))
	}
	var named []string
	for _, l := range lines {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, lotPrefix) {
			named = append(named, l)
		}
	}
	if slices.Equal(named, want) || (len(named) == 0 && len(want) == 1) {
		return nil
	}
	return fmt.Errorf("%w: want %q", ErrLots, strings.Join(want, "\n"))
}

// measuresNamed checks that an order names its supplier's measures, in
// every line it names them in, as the check reads them (measuresOf); the
// refusal tells the line wanted.
func (rv *review) measuresNamed(p proposal, supplier string, lines []string) error {
	want, err := rv.measuresOf(p.material, supplier)
	if err != nil {
		return err
	}
	named := false
	for _, l := range lines {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, measuresPrefix) {
			if l != want {
				return fmt.Errorf("%w: want %q", ErrMeasures, want)
			}
			named = true
		}
	}
	if !named {
		return fmt.Errorf("%w: want %q", ErrMeasures, want)
	}
	return nil
}

// figured is a content's lines whose numbers must be ones the run read:
// every line but an order's first, its lots and its measures, which the
// check holds whole to what it read.
func figured(lines []string) string {
	rest := make([]string, 0, len(lines))
	for _, l := range lines[1:] {
		if t := strings.TrimSpace(l); !strings.HasPrefix(t, measuresPrefix) && !strings.HasPrefix(t, lotPrefix) {
			rest = append(rest, l)
		}
	}
	return strings.Join(rest, "\n")
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
