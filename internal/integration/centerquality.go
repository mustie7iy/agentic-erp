package integration

import (
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// centerCheck carries what the controlling checks share: the extract's
// profit centers by key with their periods (nil when they did not read),
// its segments, and the texts of all of them.
type centerCheck struct {
	q        *qualityCheck
	profit   map[string]sapProfitCenter
	periods  map[string][]period[pcAttr]
	segments map[string]bool
	texts    map[string][]sapText
}

// centers checks the controlling master data (ADR-0050 §10, T56h): what
// the platform cannot hold or the load cannot read is refused before
// anything loads, rather than guessed.
func (q *qualityCheck) centers() {
	c := &centerCheck{q: q, profit: map[string]sapProfitCenter{}, periods: map[string][]period[pcAttr]{}, segments: map[string]bool{},
		texts: centerTexts(q.e)}
	for _, s := range q.e.FAGL_SEGM {
		c.segment(s)
	}
	pcs := profitCenters(q.e)
	for _, pc := range pcs {
		c.profit[pc.key()] = pc
	}
	for _, r := range q.e.CEPC_BUKRS {
		if _, ok := c.profit[r.KOKRS+"/"+r.PRCTR]; !ok {
			q.add(SevError, "CEPC_BUKRS", r.KOKRS+"/"+r.PRCTR+"/"+r.BUKRS, "profit center %s of controlling area %s is not in CEPC", r.PRCTR, r.KOKRS)
		}
	}
	c.collisions(pcs)
	for _, pc := range pcs {
		c.profitCenter(pc)
	}
	for _, cc := range costCenters(q.e) {
		c.costCenter(cc)
	}
	q.out = append(q.out, planGroups(q.e).issues...) // the center groups (T56h2)
}

// text checks that a center or segment has a name to load with (pickText),
// and warns of one only in another language than English.
func (c *centerCheck) text(table, key string) {
	t, isEnglish, ok := pickText(c.texts[table+"/"+key])
	switch {
	case !ok:
		c.q.add(SevError, table, key, "no text with a name; the platform needs a name")
	case !isEnglish:
		c.q.add(SevWarning, table, key, "no text in English; it loads named %q, its text in language %s", t.name, t.lang)
	}
}

func (c *centerCheck) segment(s sap.FAGLSegm) {
	c.segments[s.SEGMENT] = true
	if !centerCode.MatchString(s.SEGMENT) {
		c.q.add(SevError, "FAGL_SEGM", s.SEGMENT, "segment %q is not a code the platform holds (up to 10 of 0-9, A-Z, _ and -)", s.SEGMENT)
	}
	c.text("FAGL_SEGM", s.SEGMENT)
}

// collisions refuses a profit center code in more than one controlling
// area: profit centers are the tenant's, one per code (ADR-0050 §10 maps
// SAP's controlling area onto the tenant; prefixing codes per area is the
// migration lead's to decide and not built).
func (c *centerCheck) collisions(pcs []sapProfitCenter) {
	areas := map[string][]string{}
	for _, pc := range pcs {
		areas[pc.code] = append(areas[pc.code], pc.area)
	}
	for _, code := range sortedKeys(areas) {
		if a := areas[code]; len(a) > 1 {
			c.q.add(SevError, "CEPC", code, "profit center %s is in controlling areas %s; the tenant holds one profit center of a code", code, strings.Join(a, ", "))
		}
	}
}

func (c *centerCheck) profitCenter(pc sapProfitCenter) {
	key := pc.key()
	if !centerCode.MatchString(pc.code) {
		c.q.add(SevError, "CEPC", key, "profit center %q is not a code the platform holds (up to 10 of 0-9, A-Z, _ and -)", pc.code)
	}
	if ps, err := profitCenterPeriods(pc); err != nil {
		c.q.add(SevError, "CEPC", key, "%v", err)
	} else {
		c.periods[key] = ps
	}
	last := pc.slices[len(pc.slices)-1]
	for _, s := range pc.slices {
		if s.SEGMENT != last.SEGMENT {
			c.q.add(SevError, "CEPC", key, "its slices name segments %q and %q; the platform keeps one segment per profit center (a change is a reorganisation)", s.SEGMENT, last.SEGMENT)
			break
		}
	}
	if last.SEGMENT != "" && !c.segments[last.SEGMENT] {
		c.q.add(SevError, "CEPC", key, "segment %s is not in FAGL_SEGM", last.SEGMENT)
	}
	if strings.TrimSpace(last.VERAK) == "" {
		c.q.add(SevError, "CEPC", key, "no person responsible (VERAK); the platform needs one")
	}
	c.text("CEPC", key)
}

func (c *centerCheck) costCenter(cc sapCostCenter) {
	key := cc.key()
	if !centerCode.MatchString(cc.code) {
		c.q.add(SevError, "CSKS", key, "cost center %q is not a code the platform holds (up to 10 of 0-9, A-Z, _ and -)", cc.code)
	}
	last := cc.slices[len(cc.slices)-1]
	for _, s := range cc.slices {
		if s.BUKRS != last.BUKRS {
			c.q.add(SevError, "CSKS", key, "its slices are in company codes %s and %s; a platform cost center belongs to one company", s.BUKRS, last.BUKRS)
			return
		}
	}
	if strings.TrimSpace(last.VERAK) == "" {
		c.q.add(SevError, "CSKS", key, "no person responsible (VERAK); the platform needs one")
	}
	if cur := c.q.currency[last.BUKRS]; last.WAERS != "" && last.WAERS != cur {
		c.q.add(SevWarning, "CSKS", key, "currency %s is not company %s's %s; the platform keeps a cost center in its company's currency", last.WAERS, last.BUKRS, cur)
	}
	ps, err := costCenterPeriods(cc)
	if err != nil {
		c.q.add(SevError, "CSKS", key, "%v", err)
	} else {
		for _, p := range ps {
			c.costCenterPeriod(cc, p)
		}
	}
	c.text("CSKS", key)
}

// costCenterPeriod checks a period's category and profit center: an
// active category of the configuration; a profit center of the extract,
// assigned to the cost center's company and valid over the period.
func (c *centerCheck) costCenterPeriod(cc sapCostCenter, p period[ccAttr]) {
	key, company := cc.key(), cc.company()
	if !c.q.t.categories[p.attr.category] {
		c.q.add(SevError, "CSKS", key, "category %q (KOSAR) is not a cost center category of the configuration; add it to controlling.cost_center_categories",
			p.attr.category)
	}
	if p.attr.profitCenter == "" {
		return
	}
	pcKey := cc.area + "/" + p.attr.profitCenter
	pc, ok := c.profit[pcKey]
	switch {
	case !ok:
		c.q.add(SevError, "CSKS", key, "unknown profit center %s: not in CEPC of controlling area %s assigned to company %s", p.attr.profitCenter, cc.area, company)
	case !slices.Contains(pc.companies, company):
		c.q.add(SevError, "CSKS", key, "profit center %s is not assigned to company %s (CEPC_BUKRS)", p.attr.profitCenter, company)
	case c.periods[pcKey] != nil && !covers(c.periods[pcKey], p.from, p.to):
		c.q.add(SevError, "CSKS", key, "profit center %s is not valid from %s to %s", p.attr.profitCenter, p.from.Format("2006-01-02"), lastDay(p.to))
	}
}
