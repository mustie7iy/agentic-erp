package ledger_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
)

// The account assignment in the journal API (T56d2, ADR-0050 §5): lines
// take entered centers on manual and parked entries and show each center
// with its code; the list filters by center; JournalEntryPosted carries
// them.

// centeredLine is a line of a journal entry body with the centers given
// ("": none).
type centeredLine struct {
	account, amount, partner, cc, pc, seg string
}

func centeredEntry(le uuid.UUID, date string, lines ...centeredLine) string {
	parts := make([]string, len(lines))
	for i, l := range lines {
		p := fmt.Sprintf(`{"account_id": %q, "amount": {"amount": %q, "currency": "EUR"}`, l.account, l.amount)
		for field, v := range map[string]string{"partner_id": l.partner, "cost_center_id": l.cc, "profit_center_id": l.pc, "segment_id": l.seg} {
			if v != "" {
				p += fmt.Sprintf(`, %q: %q`, field, v)
			}
		}
		parts[i] = p + "}"
	}
	return fmt.Sprintf(`{"legal_entity_id": %q, "document_date": %q, "posting_date": %q, "currency": "EUR", "lines": [%s]}`,
		le, date, date, strings.Join(parts, ", "))
}

// centersOf is a line's centers with their codes as "cc:code/pc:code/seg:code",
// "-" for each one absent.
func centersOf(v any) string {
	l, _ := v.(map[string]any)
	parts := make([]string, 3)
	for i, c := range []string{"cost_center", "profit_center", "segment"} {
		parts[i] = "-"
		if id, ok := l[c+"_id"]; ok {
			parts[i] = fmt.Sprintf("%v:%v", id, l[c+"_code"])
		}
	}
	return strings.Join(parts, "/")
}

// centersAre is centersOf a line with the centers and codes given (an id
// "": none).
func centersAre(cc, ccCode, pc, pcCode, seg, segCode string) string {
	l := map[string]any{}
	for _, c := range [][3]string{{"cost_center", cc, ccCode}, {"profit_center", pc, pcCode}, {"segment", seg, segCode}} {
		if c[1] != "" {
			l[c[0]+"_id"], l[c[0]+"_code"] = c[1], c[2]
		}
	}
	return centersOf(l)
}

func noCenters() string { return "-/-/-" }

func TestJournalAPIAssignment(t *testing.T) {
	a := setupAssigned(t)
	cc2000, cc1000, pc1, dummy, seg1 := a.cc2000.String(), a.cc1000.String(), a.pc1.String(), a.dummy.String(), a.seg1.String()
	ap := func(amount string) centeredLine {
		return centeredLine{account: a.ap.String(), amount: amount, partner: a.supplier.String()}
	}
	entered, _ := a.Must(201, "POST", "/v1/journal-entries", centeredEntry(a.de, "2026-05-10",
		centeredLine{account: a.it.String(), amount: "1.00", cc: cc2000}, ap("-1.00")), "")
	derived, _ := a.Must(201, "POST", "/v1/journal-entries", centeredEntry(a.de, "2026-05-10",
		centeredLine{account: a.office.String(), amount: "2.00"}, ap("-2.00")), "")
	t.Run("posted lines", func(t *testing.T) {
		for name, c := range map[string]struct {
			entry map[string]any
			want  []string
		}{
			"an entered cost center": {entered, []string{centersAre(cc2000, "2000", dummy, "DUMMY", "", ""), noCenters()}},
			"a default cost center":  {derived, []string{centersAre(cc1000, "1000", pc1, "PC1", seg1, "SEG1"), noCenters()}},
		} {
			t.Run(name, func(t *testing.T) {
				for i, l := range apitest.List(t, c.entry, "lines") {
					if got := centersOf(l); got != c.want[i] {
						t.Errorf("line %d: got %s, want %s", i+1, got, c.want[i])
					}
				}
			})
		}
	})
	t.Run("an entered center refused", func(t *testing.T) {
		a.in(t).Problem(422, "ledger.profit_center_mismatch", "POST", "/v1/journal-entries", centeredEntry(a.de, "2026-05-10",
			centeredLine{account: a.it.String(), amount: "1.00", cc: cc1000, pc: dummy}, ap("-1.00")), "")
	})
	t.Run("list filters", func(t *testing.T) {
		for name, c := range map[string]struct {
			query string
			want  []any
		}{
			"by cost center":            {"cost_center_id=" + cc2000, []any{entered["id"]}},
			"by profit center":          {"profit_center_id=" + pc1, []any{derived["id"]}},
			"by both":                   {"cost_center_id=" + cc1000 + "&profit_center_id=" + pc1, []any{derived["id"]}},
			"by a center without lines": {"cost_center_id=" + a.cc3000.String(), nil},
		} {
			t.Run(name, func(t *testing.T) {
				page, _ := a.in(t).Must(200, "GET", fmt.Sprintf("/v1/journal-entries?legal_entity_id=%s&%s", a.de, c.query), "", "")
				var got []any
				for _, e := range apitest.List(t, page, "items") {
					got = append(got, e.(map[string]any)["id"])
				}
				if fmt.Sprint(got) != fmt.Sprint(c.want) {
					t.Errorf("got %v, want %v", got, c.want)
				}
			})
		}
	})
	t.Run("JournalEntryPosted", func(t *testing.T) {
		for _, ev := range events(t, a.ten, "journal_entry_posted") {
			if ev["journal_entry_id"] != derived["id"] {
				continue
			}
			lines := ev["lines"].([]any)
			if got, want := centersOf(lines[0]), centersAre(cc1000, "1000", pc1, "PC1", seg1, "SEG1"); got != want {
				t.Errorf("line 1: got %s, want %s", got, want)
			}
			if got := centersOf(lines[1]); got != noCenters() {
				t.Errorf("line 2: got %s, want %s", got, noCenters())
			}
			return
		}
		t.Errorf("no JournalEntryPosted for entry %v", derived["id"])
	})
}

// TestParkedEntryAssignment: a parked entry keeps the centers entered on
// its lines, through a change of its header, and its posting derives the
// rest.
func TestParkedEntryAssignment(t *testing.T) {
	a := setupAssigned(t)
	cc2000, pc1, seg1, dummy := a.cc2000.String(), a.pc1.String(), a.seg1.String(), a.dummy.String()
	pe, tag := a.Must(201, "POST", "/v1/parked-journal-entries", centeredEntry(a.de, "2026-05-10",
		centeredLine{account: a.it.String(), amount: "1.00", cc: cc2000},
		centeredLine{account: a.bank.String(), amount: "-1.00", pc: pc1, seg: seg1}), "")
	path := "/v1/parked-journal-entries/" + pe["id"].(string)
	want := []string{centersAre(cc2000, "2000", "", "", "", ""), centersAre("", "", pc1, "PC1", seg1, "SEG1")}
	check := func(t *testing.T, what string, lines []any, want []string) {
		t.Helper()
		for i, l := range lines {
			if got := centersOf(l); got != want[i] {
				t.Errorf("%s line %d: got %s, want %s", what, i+1, got, want[i])
			}
		}
	}
	check(t, "parked", apitest.List(t, pe, "lines"), want)
	pe, tag = a.Must(200, "PATCH", path, `{"header_text": "Accrual"}`, tag)
	check(t, "changed", apitest.List(t, pe, "lines"), want)
	posted, _ := a.Must(200, "POST", path+":post", "", tag)
	je, _ := a.Must(200, "GET", fmt.Sprintf("/v1/journal-entries/%v", posted["journal_entry_id"]), "", "")
	check(t, "posted", apitest.List(t, je, "lines"), []string{centersAre(cc2000, "2000", dummy, "DUMMY", "", ""), centersAre("", "", pc1, "PC1", seg1, "SEG1")})
}
