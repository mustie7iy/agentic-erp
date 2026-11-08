package integration

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// Controlling master data (ADR-0050 §10, T56h): SAP's segments
// (FAGL_SEGM/FAGL_SEGMT), profit centers (CEPC/CEPCT/CEPC_BUKRS) and cost
// centers (CSKS/CSKT) load as the ledger's segments and centers, each
// center with its time slices as its periods. Every table and field name,
// and what each is taken to mean (internal/sap), is pending the SAP
// specialist (docs/migration.md).

// centerCode is the form of a center's and a segment's code on the
// platform (ADR-0050 §3, SAP KOSTL/PRCTR).
var centerCode = regexp.MustCompile(`^[0-9A-Z_-]{1,10}$`)

// sapOpenEnd is DATBI of a slice without an end.
const sapOpenEnd = "99991231"

// english are the language keys a text is taken in English by: SAP's
// language key E, and EN as an ISO 639-1 code. The platform has no tenant
// language yet, so texts load in English, else in another language with a
// warning (ADR-0050 §10).
var english = map[string]bool{"E": true, "EN": true}

// period is a center's slice as SAP holds it: from its first day to the
// day after its last (zero: open-ended), with what it says (ccAttr,
// pcAttr).
type period[A comparable] struct {
	from, to time.Time
	attr     A
}

// ccAttr is what a cost center's slice says that the platform keeps.
type ccAttr struct {
	category, profitCenter          string // the profit center by its code (PRCTR)
	lockCost, lockRevenue, lockPlan bool
}

// pcAttr is what a profit center's slice says that the platform keeps.
type pcAttr struct{ locked bool }

// sapValidity reads a slice's DATAB and DATBI, both days included, as the
// platform's validity, valid_to exclusive and zero for DATBI 99991231.
func sapValidity(datab, datbi string) (from, to time.Time, err error) {
	if from, err = sapDate(datab); err != nil {
		return from, to, fmt.Errorf("DATAB: %w", err)
	}
	if datbi == sapOpenEnd {
		return from, to, nil
	}
	if to, err = sapDate(datbi); err != nil {
		return from, to, fmt.Errorf("DATBI: %w", err)
	}
	if to.Before(from) {
		return from, to, fmt.Errorf("DATBI %s is before DATAB %s", datbi, datab)
	}
	return from, to.AddDate(0, 0, 1), nil
}

// sapFlag reads a one-character indicator: "X" set, "" not.
func sapFlag(field, v string) (bool, error) {
	switch v {
	case "X":
		return true, nil
	case "":
		return false, nil
	}
	return false, fmt.Errorf("%s %q is neither X nor blank", field, v)
}

// joinPeriods checks that the slices adjoin, each starting where the one
// before ends, and joins neighbours that say the same: what SAP keeps in a
// slice that the platform does not (a text, the person responsible) makes
// no period of its own.
func joinPeriods[A comparable](in []period[A]) ([]period[A], error) {
	var out []period[A]
	for i, p := range in {
		if i > 0 && (in[i-1].to.IsZero() || !in[i-1].to.Equal(p.from)) {
			return nil, fmt.Errorf("its slices do not adjoin: one starting %s follows one ending %s", p.from.Format(time.DateOnly), lastDay(in[i-1].to))
		}
		if n := len(out); n > 0 && out[n-1].attr == p.attr {
			out[n-1].to = p.to
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// lastDay is a slice's last day as SAP says it, for messages.
func lastDay(to time.Time) string {
	if to.IsZero() {
		return sapOpenEnd
	}
	return to.AddDate(0, 0, -1).Format(time.DateOnly)
}

// covers reports whether the periods, at least one, are valid over
// [from, to).
func covers[A comparable](ps []period[A], from, to time.Time) bool {
	end, last := endOrMax(to), endOrMax(ps[len(ps)-1].to)
	return !from.Before(ps[0].from) && !end.After(last)
}

func endOrMax(t time.Time) time.Time {
	if t.IsZero() {
		return time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)
	}
	return t
}

// sapCostCenter is a cost center of the extract with its slices in date
// order.
type sapCostCenter struct {
	area, code string
	slices     []sap.CSKS
}

func (c sapCostCenter) key() string { return c.area + "/" + c.code }

// company is the company code of its last slice, the one it is loaded in.
func (c sapCostCenter) company() string { return c.slices[len(c.slices)-1].BUKRS }

// sapProfitCenter is a profit center of the extract with its slices in date
// order and the company codes it is assigned to (CEPC_BUKRS), sorted.
type sapProfitCenter struct {
	area, code string
	slices     []sap.CEPC
	companies  []string
}

func (c sapProfitCenter) key() string { return c.area + "/" + c.code }

// costCenters groups the extract's CSKS by cost center, sorted by key.
func costCenters(e *sap.Extract) []sapCostCenter {
	by := map[string]*sapCostCenter{}
	for _, r := range e.CSKS {
		k := r.KOKRS + "/" + r.KOSTL
		if by[k] == nil {
			by[k] = &sapCostCenter{area: r.KOKRS, code: r.KOSTL}
		}
		by[k].slices = append(by[k].slices, r)
	}
	out := make([]sapCostCenter, 0, len(by))
	for _, k := range sortedKeys(by) {
		c := *by[k]
		slices.SortStableFunc(c.slices, func(a, b sap.CSKS) int { return cmp.Compare(a.DATAB, b.DATAB) })
		out = append(out, c)
	}
	return out
}

// profitCenters groups the extract's CEPC by profit center, with their
// company codes, sorted by key.
func profitCenters(e *sap.Extract) []sapProfitCenter {
	by := map[string]*sapProfitCenter{}
	for _, r := range e.CEPC {
		k := r.KOKRS + "/" + r.PRCTR
		if by[k] == nil {
			by[k] = &sapProfitCenter{area: r.KOKRS, code: r.PRCTR}
		}
		by[k].slices = append(by[k].slices, r)
	}
	for _, r := range e.CEPC_BUKRS {
		if c := by[r.KOKRS+"/"+r.PRCTR]; c != nil && !slices.Contains(c.companies, r.BUKRS) {
			c.companies = append(c.companies, r.BUKRS)
		}
	}
	out := make([]sapProfitCenter, 0, len(by))
	for _, k := range sortedKeys(by) {
		c := *by[k]
		slices.SortStableFunc(c.slices, func(a, b sap.CEPC) int { return cmp.Compare(a.DATAB, b.DATAB) })
		slices.Sort(c.companies)
		out = append(out, c)
	}
	return out
}

// costCenterPeriods are a cost center's slices as the platform's periods.
func costCenterPeriods(c sapCostCenter) ([]period[ccAttr], error) {
	ps := make([]period[ccAttr], 0, len(c.slices))
	for _, s := range c.slices {
		from, to, err := sapValidity(s.DATAB, s.DATBI)
		if err != nil {
			return nil, err
		}
		a := ccAttr{category: s.KOSAR, profitCenter: s.PRCTR}
		for _, f := range []struct {
			name, value string
			into        *bool
		}{{"BKZKP", s.BKZKP, &a.lockCost}, {"BKZER", s.BKZER, &a.lockRevenue}, {"PKZKP", s.PKZKP, &a.lockPlan}} {
			if *f.into, err = sapFlag(f.name, f.value); err != nil {
				return nil, err
			}
		}
		ps = append(ps, period[ccAttr]{from: from, to: to, attr: a})
	}
	return joinPeriods(ps)
}

// profitCenterPeriods are a profit center's slices as the platform's
// periods.
func profitCenterPeriods(c sapProfitCenter) ([]period[pcAttr], error) {
	ps := make([]period[pcAttr], 0, len(c.slices))
	for _, s := range c.slices {
		from, to, err := sapValidity(s.DATAB, s.DATBI)
		if err != nil {
			return nil, err
		}
		locked, err := sapFlag("LOCK_IND", s.LOCK_IND)
		if err != nil {
			return nil, err
		}
		ps = append(ps, period[pcAttr]{from: from, to: to, attr: pcAttr{locked: locked}})
	}
	return joinPeriods(ps)
}

// sapText is a center's or segment's text in one language, of the slice
// ending on datbi ("" for a segment's).
type sapText struct {
	lang, datbi, name, description string
}

// pickText is the text a center or segment loads with: in English, else in
// the first other language by key (english is false then), of the latest
// slice. ok is false when there is none with a name.
func pickText(texts []sapText) (t sapText, isEnglish, ok bool) {
	texts = slices.DeleteFunc(slices.Clone(texts), func(t sapText) bool { return strings.TrimSpace(t.name) == "" })
	if len(texts) == 0 {
		return sapText{}, false, false
	}
	slices.SortFunc(texts, func(a, b sapText) int {
		if ea, eb := english[a.lang], english[b.lang]; ea != eb {
			if ea {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(a.lang, b.lang), cmp.Compare(b.datbi, a.datbi))
	})
	return texts[0], english[texts[0].lang], true
}

// centerTexts are the texts of the extract's centers and segments by
// table and key: CSKT and CEPCT by KOKRS/code, FAGL_SEGMT by segment.
func centerTexts(e *sap.Extract) map[string][]sapText {
	out := map[string][]sapText{}
	for _, t := range e.CSKT {
		k := "CSKS/" + t.KOKRS + "/" + t.KOSTL
		out[k] = append(out[k], sapText{lang: t.SPRAS, datbi: t.DATBI, name: t.KTEXT, description: t.LTEXT})
	}
	for _, t := range e.CEPCT {
		k := "CEPC/" + t.KOKRS + "/" + t.PRCTR
		out[k] = append(out[k], sapText{lang: t.SPRAS, datbi: t.DATBI, name: t.KTEXT, description: t.LTEXT})
	}
	for _, t := range e.FAGL_SEGMT {
		k := "FAGL_SEGM/" + t.SEGMENT
		out[k] = append(out[k], sapText{lang: t.LANGU, name: t.NAME})
	}
	return out
}
