package ledger_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Trading partners on journal lines (T68b, ADR-0059 §4): entered on
// manual, parked and template lines, given through the posting port,
// copied by reversals and clearings, refused when they are no other
// company of the tenant, shown by code and name, filtered on, carried by
// JournalEntryPosted and added to ledger.trading_partner_balance.

// partnerLine is a line of an entry body with a trading partner ("":
// none).
type partnerLine struct {
	account, amount, partner, tradingPartner string
}

func partnerEntry(le uuid.UUID, date string, lines ...partnerLine) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "document_date": %q, "posting_date": %q, "currency": "EUR", "lines": %s}`, le, date, date, partnerLines(lines...))
}

func partnerLines(lines ...partnerLine) string {
	parts := make([]string, len(lines))
	for i, l := range lines {
		p := fmt.Sprintf(`{"account_id": %q, "amount": {"amount": %q, "currency": "EUR"}`, l.account, l.amount)
		if l.partner != "" {
			p += fmt.Sprintf(`, "partner_id": %q`, l.partner)
		}
		if l.tradingPartner != "" {
			p += fmt.Sprintf(`, "trading_partner_id": %q`, l.tradingPartner)
		}
		parts[i] = p + "}"
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// tradingPartnersOf is each line's trading partner as "id code name", "-"
// for none, joined by " | ".
func tradingPartnersOf(tb testing.TB, rep map[string]any) string {
	tb.Helper()
	var out []string
	for _, l := range apitest.List(tb, rep, "lines") {
		tp, ok := l.(map[string]any)["trading_partner"].(map[string]any)
		if !ok {
			out = append(out, "-")
			continue
		}
		out = append(out, fmt.Sprintf("%v %v %v", tp["id"], tp["code"], tp["name"]))
	}
	return strings.Join(out, " | ")
}

func TestTradingPartnerOnManualEntries(t *testing.T) {
	e := setup(t)
	bank, revenue := e.account(t, "2000", "100000"), e.account(t, "2000", "400000")
	us := e.us.String()
	named := us + " 1000 Acme Corp US"
	je, tag := e.Must(201, "POST", "/v1/journal-entries", partnerEntry(e.de, "2026-03-15",
		partnerLine{account: bank, amount: "100.00", tradingPartner: us}, partnerLine{account: revenue, amount: "-60.00", tradingPartner: us},
		partnerLine{account: revenue, amount: "-40.00"}), "")
	id := je["id"].(string)
	t.Run("posted lines carry the trading partner by code and name", func(t *testing.T) {
		if got, want := tradingPartnersOf(t, je), named+" | "+named+" | -"; got != want {
			t.Errorf("posted: got %q, want %q", got, want)
		}
	})
	t.Run("a read gives it, and renaming the company changes no ETag", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE tenancy.legal_entity SET name = 'Acme Corporation US', version = version + 1 WHERE id = $1`, e.us)
		got, gotTag := e.in(t).Must(200, "GET", "/v1/journal-entries/"+id, "", "")
		if want := us + " 1000 Acme Corporation US | " + us + " 1000 Acme Corporation US | -"; tradingPartnersOf(t, got) != want {
			t.Errorf("read: got %q, want %q", tradingPartnersOf(t, got), want)
		}
		if gotTag != tag {
			t.Errorf("ETag after the rename: got %s, want %s", gotTag, tag)
		}
		pgtest.Exec(t, `UPDATE tenancy.legal_entity SET name = 'Acme Corp US', version = version + 1 WHERE id = $1`, e.us)
	})
	t.Run("the list filters by trading partner", func(t *testing.T) {
		e.Must(201, "POST", "/v1/journal-entries", partnerEntry(e.de, "2026-03-15", partnerLine{account: bank, amount: "1.00"},
			partnerLine{account: revenue, amount: "-1.00"}), "")
		for name, c := range map[string]struct {
			partner string
			want    []string
		}{"the US company": {us, []string{id}}, "another": {uuid.NewString(), nil}} {
			t.Run(name, func(t *testing.T) {
				page, _ := e.in(t).Must(200, "GET", "/v1/journal-entries?legal_entity_id="+e.de.String()+"&trading_partner_id="+c.partner, "", "")
				var got []string
				for _, it := range apitest.List(t, page, "items") {
					got = append(got, it.(map[string]any)["id"].(string))
					if c.want != nil && tradingPartnersOf(t, it.(map[string]any)) != named+" | "+named+" | -" {
						t.Errorf("listed lines: got %q", tradingPartnersOf(t, it.(map[string]any)))
					}
				}
				if fmt.Sprint(got) != fmt.Sprint(c.want) {
					t.Errorf("entries: got %v, want %v", got, c.want)
				}
			})
		}
	})
	t.Run("JournalEntryPosted carries it", func(t *testing.T) {
		var got []any
		for _, ev := range events(t, e.ten, "journal_entry_posted") {
			if ev["journal_entry_id"] == id {
				for _, l := range ev["lines"].([]any) {
					got = append(got, l.(map[string]any)["trading_partner_id"])
				}
			}
		}
		if want := []any{us, us, nil}; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("event lines' trading partners: got %v, want %v", got, want)
		}
	})
	t.Run("the reversal copies each line's", func(t *testing.T) {
		rev, _ := e.in(t).Must(200, "POST", "/v1/journal-entries/"+id+":reverse", "", tag)
		if got, want := tradingPartnersOf(t, rev), named+" | "+named+" | -"; got != want {
			t.Errorf("reversal: got %q, want %q", got, want)
		}
	})
	t.Run("the projection adds the lines with one to their balances", func(t *testing.T) {
		fixture.Project(t)
		if got, want := tradingPartnerBalances(t, e), []string{
			fmt.Sprintf("%s/%s 2026/3 EUR 10000/10000 10000/10000", bank, us), fmt.Sprintf("%s/%s 2026/3 EUR 6000/6000 6000/6000", revenue, us)}; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("balances: got %v, want %v", got, want)
		}
	})
}

// tradingPartnerBalances are the tenant's trading partner balances as
// "account/partner year/period currency debit_lc/credit_lc
// debit_gc/credit_gc", by account and partner.
func tradingPartnerBalances(t *testing.T, e env) []string {
	t.Helper()
	rows, _ := pgtest.Admin(t).Query(context.Background(), `SELECT account_id::text, trading_partner_id::text, fiscal_year, fiscal_period, currency_tc,
			debit_lc, credit_lc, debit_gc, credit_gc
		FROM ledger.trading_partner_balance WHERE tenant_id = $1 ORDER BY debit_lc DESC, account_id`, e.ten.ID)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var account, partner, currency string
		var year, period int
		var dl, cl, dg, cg int64
		if err := rows.Scan(&account, &partner, &year, &period, &currency, &dl, &cl, &dg, &cg); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s/%s %d/%d %s %d/%d %d/%d", account, partner, year, period, currency, dl, cl, dg, cg))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTradingPartnerRefused(t *testing.T) {
	e := setup(t)
	bank, revenue := e.account(t, "2000", "100000"), e.account(t, "2000", "400000")
	other := pgtest.NewTenant(t)
	foreign := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, '9000', 'Elsewhere Ltd', 'US', 'USD', $3, 'active', $4, $4)`, other.ID, foreign, uuid.New(), other.Human.ID)
	for name, c := range map[string]struct {
		partner, code string
	}{
		"the entry's own company":       {e.de.String(), "own_company"},
		"another tenant's company":      {foreign.String(), "unknown"},
		"an id that is no legal entity": {uuid.NewString(), "unknown"},
	} {
		body := partnerEntry(e.de, "2026-03-15", partnerLine{account: bank, amount: "1.00", tradingPartner: e.us.String()},
			partnerLine{account: revenue, amount: "-1.00", tradingPartner: c.partner})
		for _, path := range []string{"/v1/journal-entries", "/v1/parked-journal-entries"} {
			t.Run(name+" on "+path, func(t *testing.T) {
				p := e.in(t).Problem(422, "ledger.trading_partner_invalid", "POST", path, body, "")
				if got := fieldErrors(t, p); got != "lines[1].trading_partner_id:"+c.code {
					t.Errorf("field errors: got %q, want lines[1].trading_partner_id:%s", got, c.code)
				}
			})
		}
	}
}

// fieldErrors is a problem's field errors as "field:code", joined by " ".
func fieldErrors(tb testing.TB, p map[string]any) string {
	tb.Helper()
	var out []string
	for _, fe := range apitest.List(tb, p, "errors") {
		m := fe.(map[string]any)
		out = append(out, fmt.Sprintf("%v:%v", m["field"], m["code"]))
	}
	return strings.Join(out, " ")
}

func TestTradingPartnerOnParkedEntriesAndTemplates(t *testing.T) {
	e := setup(t)
	bank, revenue := e.account(t, "2000", "100000"), e.account(t, "2000", "400000")
	us := e.us.String()
	named := us + " 1000 Acme Corp US | -"
	lines := []partnerLine{{account: bank, amount: "5.00", tradingPartner: us}, {account: revenue, amount: "-5.00"}}
	parked, _ := e.Must(201, "POST", "/v1/parked-journal-entries", partnerEntry(e.de, "2026-03-15", lines...), "")
	pid := parked["id"].(string)
	template, _ := e.Must(201, "POST", "/v1/entry-templates", fmt.Sprintf(`{"legal_entity_id": %q, "name": "Intercompany charge", "currency": "EUR", "lines": %s}`,
		e.de, partnerLines(lines...)), "")
	tid := template["id"].(string)
	for name, c := range map[string]struct {
		method, path, body, ifMatch string
		status                      int
		items                       bool
	}{
		"parked":           {"GET", "/v1/parked-journal-entries/" + pid, "", "", 200, false},
		"parked list":      {"GET", "/v1/parked-journal-entries?legal_entity_id=" + e.de.String(), "", "", 200, true},
		"template":         {"GET", "/v1/entry-templates/" + tid, "", "", 200, false},
		"template list":    {"GET", "/v1/entry-templates?legal_entity_id=" + e.de.String(), "", "", 200, true},
		"parked from it":   {"POST", "/v1/entry-templates/" + tid + ":park", `{"posting_date": "2026-03-20"}`, "*", 201, false},
		"template changed": {"PATCH", "/v1/entry-templates/" + tid, `{"lines": ` + partnerLines(lines...) + `}`, "*", 200, false},
		"parked changed":   {"PATCH", "/v1/parked-journal-entries/" + pid, `{"lines": ` + partnerLines(lines...) + `}`, "*", 200, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, _ := e.in(t).Must(c.status, c.method, c.path, c.body, c.ifMatch)
			reps := []any{got}
			if c.items {
				reps = apitest.List(t, got, "items")
			}
			for _, r := range reps {
				if tradingPartnersOf(t, r.(map[string]any)) != named {
					t.Errorf("lines: got %q, want %q", tradingPartnersOf(t, r.(map[string]any)), named)
				}
			}
		})
	}
	t.Run("posting the parked entry posts its trading partner", func(t *testing.T) {
		e := e.in(t)
		_, tag := e.Must(200, "GET", "/v1/parked-journal-entries/"+pid, "", "")
		posted, _ := e.Must(200, "POST", "/v1/parked-journal-entries/"+pid+":post", "", tag)
		je, _ := e.Must(200, "GET", "/v1/journal-entries/"+posted["journal_entry_id"].(string), "", "")
		if got := tradingPartnersOf(t, je); got != named {
			t.Errorf("posted lines: got %q, want %q", got, named)
		}
		if got := tradingPartnersOf(t, posted); got != named {
			t.Errorf("parked lines after posting: got %q, want %q", got, named)
		}
	})
	t.Run("a kept line whose trading partner is no legal entity fails the read", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE ledger.entry_template_line SET trading_partner_id = $2 WHERE template_id = $1 AND line_no = 1`, uuid.MustParse(tid), uuid.New())
		if code, _, _ := e.in(t).Do("GET", "/v1/entry-templates/"+tid, "", ""); code != 500 {
			t.Errorf("status: got %d, want 500", code)
		}
	})
}

func TestTradingPartnerThroughThePostingPort(t *testing.T) {
	e := setup(t)
	expense, ap := settleAccount(t, e, "1000", "520000"), settleAccount(t, e, "1000", "211000")
	settleRate(t, e, "USD", "2026-03-01", "0.90")
	settleRate(t, e, "USD", "2026-04-01", "0.80")
	gain := settleAccount(t, e, "1000", "400000")
	pgtest.Exec(t, `INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1, $2, 'fx_gain', $3, $4)`,
		e.ten.ID, e.us, gain, e.ten.Human.ID)
	de, other := e.de, uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, '3000', 'Maple', 'CA', 'CAD', $3, 'active', $4, $4)`, e.ten.ID, other, e.usCal, e.ten.Human.ID)
	invoice := func(posting *uuid.UUID, line *uuid.UUID) ledger.Posting {
		return ledger.Posting{LegalEntityID: e.us, DocumentType: "SA", DocumentDate: date("2026-03-01"), PostingDate: date("2026-03-01"), Currency: "USD",
			SourceContext: "payables", SourceDocumentID: uuid.New(), TradingPartner: posting,
			Lines: []ledger.PostingLine{{AccountID: expense, Amount: 10000, TradingPartner: line}, {AccountID: ap, Amount: -10000, PartnerID: &e.supplier}}}
	}
	var item ledger.ItemRef
	partners := func(entry ledger.Entry) string {
		var out []string
		for _, l := range entry.Lines {
			switch {
			case l.TradingPartnerID == nil:
				out = append(out, "-")
			case *l.TradingPartnerID == de:
				out = append(out, "de")
			default:
				out = append(out, "other")
			}
		}
		return strings.Join(out, " ")
	}
	t.Run("a line's own, else the posting's", func(t *testing.T) {
		err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			entry, err := postings().Post(ctx, invoice(&de, &other))
			if err != nil {
				return err
			}
			if got := partners(entry); got != "other de" {
				t.Errorf("lines: got %q, want %q", got, "other de")
			}
			item = ledger.ItemRef{FiscalYear: entry.FiscalYear, EntryID: entry.ID, LineNo: 2}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the posting's own company is refused", func(t *testing.T) {
		us := e.us
		err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			_, err := postings().Post(ctx, invoice(&us, nil))
			return err
		})
		if got := problem.From(err).Code; got != "ledger.trading_partner_invalid" {
			t.Errorf("error: got %s (%v), want ledger.trading_partner_invalid", got, err)
		}
	})
	t.Run("a settlement offsets the item with its trading partner, and its exchange difference takes it", func(t *testing.T) {
		err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			entry, err := postings().Settle(ctx, settlementOf(t, e, e.us, "1000", "USD", "2026-04-15", item))
			if err != nil {
				return err
			}
			// The item's offset, the bank line, the gain.
			if got := partners(entry); got != "de - de" {
				t.Errorf("lines: got %q, want %q", got, "de - de")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestTradingPartnerOnGRIRPostings(t *testing.T) {
	g := grirSetup(t)
	order := uuid.New()
	p := price{unit: 1000, per: 1}
	g.post(t, g.receiptPosting("2026-03-10", "EUR", order, "-2", p))
	invoice := g.invoicePosting("2026-03-12", "EUR", order, "2", p, 2000)
	t.Run("the posting's own company is refused", func(t *testing.T) {
		in := invoice
		in.TradingPartner = &g.de
		err := apitest.InRequest(t, g.ten, g.ten.Human, func(ctx context.Context) error {
			_, err := postings().Post(ctx, in)
			return err
		})
		if got := problem.From(err).Code; got != "ledger.trading_partner_invalid" {
			t.Errorf("error: got %s (%v), want ledger.trading_partner_invalid", got, err)
		}
	})
	t.Run("its own lines carry it", func(t *testing.T) {
		in := invoice
		in.TradingPartner = &g.us
		entry := g.post(t, in)
		if got := entry.Lines[0].TradingPartnerID; got == nil || *got != g.us {
			t.Errorf("payables line: got %v, want %s", got, g.us)
		}
		if got := entry.Lines[1].TradingPartnerID; got != nil {
			t.Errorf("GR/IR line: got %v, want none", *got)
		}
	})
}

func TestTradingPartnerOnClearings(t *testing.T) {
	e := setup(t)
	bank, ap := e.account(t, "2000", "100000"), e.account(t, "2000", "211000")
	us, supplier := e.us.String(), e.supplier.String()
	inv, _ := e.Must(201, "POST", "/v1/journal-entries", partnerEntry(e.de, "2026-03-15", partnerLine{account: bank, amount: "5.00"},
		partnerLine{account: ap, amount: "-5.00", partner: supplier, tradingPartner: us}), "")
	pay, _ := e.Must(201, "POST", "/v1/journal-entries", partnerEntry(e.de, "2026-03-16", partnerLine{account: ap, amount: "5.00", partner: supplier},
		partnerLine{account: bank, amount: "-5.00"}), "")
	cl, _ := e.Must(201, "POST", "/v1/clearings", clearing(e.de, "2026-03-31", ref{inv, 2}, ref{pay, 1}), "")
	je, _ := e.Must(200, "GET", "/v1/journal-entries/"+cl["clearing_entry_id"].(string), "", "")
	// Each line offsets an item and takes its trading partner.
	if got, want := tradingPartnersOf(t, je), us+" 1000 Acme Corp US | -"; got != want {
		t.Errorf("clearing lines: got %q, want %q", got, want)
	}
}

// TestTradingPartnerDatabaseFailures calls the reads and writes that name
// trading partners with each database statement failing in turn.
func TestTradingPartnerDatabaseFailures(t *testing.T) {
	e := setup(t)
	h := endpoints()
	bank, revenue := uuid.MustParse(e.account(t, "2000", "100000")), uuid.MustParse(e.account(t, "2000", "400000"))
	lines := []gen.JournalLineInput{{AccountID: bank, Amount: gen.Money{Amount: "1.00", Currency: "EUR"}, TradingPartnerID: gen.NewOptUUID(e.us)},
		{AccountID: revenue, Amount: gen.Money{Amount: "-1.00", Currency: "EUR"}}}
	create := &gen.JournalEntryCreate{LegalEntityID: e.de, DocumentDate: date("2026-03-15"), PostingDate: date("2026-03-15"), Currency: "EUR", Lines: lines}
	body := partnerEntry(e.de, "2026-03-15", partnerLine{account: bank.String(), amount: "1.00", tradingPartner: e.us.String()},
		partnerLine{account: revenue.String(), amount: "-1.00"})
	je, _ := e.Must(201, "POST", "/v1/journal-entries", body, "")
	parked, _ := e.Must(201, "POST", "/v1/parked-journal-entries", body, "")
	template, _ := e.Must(201, "POST", "/v1/entry-templates", fmt.Sprintf(`{"legal_entity_id": %q, "name": "T", "currency": "EUR", "lines": %s}`,
		e.de, partnerLines(partnerLine{account: bank.String(), amount: "1.00", tradingPartner: e.us.String()}, partnerLine{account: revenue.String(), amount: "-1.00"})), "")
	jid, pid, tid := uuid.MustParse(je["id"].(string)), uuid.MustParse(parked["id"].(string)), uuid.MustParse(template["id"].(string))
	for name, call := range map[string]func(ctx context.Context) error{
		"post": func(ctx context.Context) error {
			_, err := h.PostJournalEntry(ctx, create, gen.PostJournalEntryParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetJournalEntry(ctx, gen.GetJournalEntryParams{ID: jid})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListJournalEntries(ctx, gen.ListJournalEntriesParams{TradingPartnerID: gen.NewOptUUID(e.us)})
			return err
		},
		"reverse": func(ctx context.Context) error {
			_, err := h.ReverseJournalEntry(ctx, gen.OptJournalEntryReverse{}, gen.ReverseJournalEntryParams{ID: jid, IfMatch: "*"})
			return err
		},
		"park": func(ctx context.Context) error {
			_, err := h.ParkJournalEntry(ctx, create, gen.ParkJournalEntryParams{})
			return err
		},
		"get parked": func(ctx context.Context) error {
			_, err := h.GetParkedJournalEntry(ctx, gen.GetParkedJournalEntryParams{ID: pid})
			return err
		},
		"list parked": func(ctx context.Context) error {
			_, err := h.ListParkedJournalEntries(ctx, gen.ListParkedJournalEntriesParams{})
			return err
		},
		"change parked": func(ctx context.Context) error {
			_, err := h.UpdateParkedJournalEntry(ctx, &gen.ParkedJournalEntryUpdate{Lines: lines}, gen.UpdateParkedJournalEntryParams{ID: pid, IfMatch: "*"})
			return err
		},
		"create template": func(ctx context.Context) error {
			_, err := h.CreateEntryTemplate(ctx, &gen.EntryTemplateCreate{LegalEntityID: e.de, Name: "T2", Currency: "EUR", Lines: lines}, gen.CreateEntryTemplateParams{})
			return err
		},
		"get template": func(ctx context.Context) error {
			_, err := h.GetEntryTemplate(ctx, gen.GetEntryTemplateParams{ID: tid})
			return err
		},
		"list templates": func(ctx context.Context) error {
			_, err := h.ListEntryTemplates(ctx, gen.ListEntryTemplatesParams{})
			return err
		},
		"change template": func(ctx context.Context) error {
			_, err := h.UpdateEntryTemplate(ctx, &gen.EntryTemplateUpdate{Lines: lines}, gen.UpdateEntryTemplateParams{ID: tid, IfMatch: "*"})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) { apitest.EveryFailure(t, e.ten, e.ten.Human, name, call) })
	}
}
