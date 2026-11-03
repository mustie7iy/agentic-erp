package web

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/net/html"

	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// postedIn is the journal entry a post's answer goes to.
func postedIn(t *testing.T, r response) uuid.UUID {
	t.Helper()
	return wentTo(t, r, "/journal/entries/", "?done=posted")
}

// wentTo is the id of the page an answer goes to, under prefix with the
// notice suffix.
func wentTo(t *testing.T, r response, prefix, suffix string) uuid.UUID {
	t.Helper()
	rest, ok := strings.CutPrefix(r.location(), prefix)
	id, err := uuid.Parse(strings.TrimSuffix(rest, suffix))
	if r.status != http.StatusSeeOther || !ok || err != nil || !strings.HasSuffix(rest, suffix) {
		t.Fatalf("got %d to %q, want %s…%s: %s", r.status, r.location(), prefix, suffix, r.body)
	}
	return id
}

// byID is the element with the id, nil for none.
func byID(p page, id string) *html.Node {
	var out *html.Node
	walk(p.doc, func(n *html.Node) {
		if attrOf(n, "id") == id {
			out = n
		}
	})
	return out
}

// asHTMX are the form's fields as htmx sends them with the op, its CSRF
// token in its header alone, and the headers it sends.
func asHTMX(v url.Values, op string) (url.Values, []string) {
	out := url.Values{}
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	token := out.Get("csrf_token")
	out.Del("csrf_token")
	out.Set("op", op)
	return out, []string{"HX-Request", "true", "X-CSRF-Token", token}
}

// TestPostAtOnce: from the form, an entry within company 2000's limit is
// posted at once and the person goes to it; one above the limit is
// parked for them (ledger.approval_required), which its page says, and
// the same form sent again parks it once; a post the API refuses is said
// on the form, what was typed kept.
func TestPostAtOnce(t *testing.T) {
	s, d := journalStack(t)
	b := s.as(d.accountant)
	p := b.get("/journal/new?legal_entity_id=" + d.f.DE.String()).page(t)
	if b := byID(p, "entry-post-button"); b == nil || !strings.Contains(text(b), "Post the entry") {
		t.Error("the form: no button to post the entry at once")
	}
	v := withLines(newEntryForm(b, d, url.Values{"header_text": {"Posted at once"}}), pair("40.00")...)
	v.Set("op", "post")
	id := postedIn(t, b.do(http.MethodPost, "/journal/new", v))
	holds(t, "posted", b.get(entryPath(id)+"?done=posted").page(t).mainText(), "The entry is posted: this is it.", "Posted at once")
	above := withLines(newEntryForm(b, d, url.Values{"header_text": {"Year-end accrual"}}), pair("12000.00")...)
	above.Set("op", "post")
	r := b.do(http.MethodPost, "/journal/new", above)
	wentTo(t, r, "/journal/parked/", "?done=limit")
	holds(t, "above the limit", b.get(r.location()).page(t).mainText(), "The entry is above the company's limit for posting at once", "Status Parked")
	if again := b.do(http.MethodPost, "/journal/new", above); again.location() != r.location() {
		t.Errorf("the same form sent again: got %q, want the entry parked once, %q", again.location(), r.location())
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.parked_entry WHERE tenant_id = $1 AND header_text = 'Year-end accrual'`, []any{d.f.Tenant.ID}, &n)
	if n != 1 {
		t.Errorf("parked entries above the limit: got %d, want 1", n)
	}
	refused := withLines(newEntryForm(b, d, nil), typedLine{account: "520000", amount: "10.00", text: "Kept"}, typedLine{account: "100000", amount: "-9.00"})
	refused.Set("op", "post")
	r = b.do(http.MethodPost, "/journal/new", refused)
	status(t, "unbalanced", r, http.StatusUnprocessableEntity)
	rp := r.page(t)
	holds(t, "unbalanced", rp.mainText(), "The entry was not saved", "ledger.unbalanced", "Difference EUR 1.00")
	if got := formValues(rp.form("/journal/new")).Get("line_text"); got != "Kept" {
		t.Errorf("a refused post: the line's text %q, want what was typed", got)
	}
}

// TestCompanyChosenOnTheForm: company 2000's form holds its currency and
// day, filled in for it; the company changed in the browser without
// htmx is not sent for the other company's currency but rendered for it,
// said so, and parked when sent again; with htmx, choosing the company
// renders the form for it at once.
func TestCompanyChosenOnTheForm(t *testing.T) {
	s, d := journalStack(t)
	b := s.as(d.accountant)
	v := withLines(newEntryForm(b, d, nil), pair("5.00")...)
	if v.Get("filled_for") != d.f.DE.String() || v.Get("currency") != "EUR" {
		t.Fatalf("company 2000's form: filled in for %q in %q, want company 2000's EUR", v.Get("filled_for"), v.Get("currency"))
	}
	v.Set("legal_entity_id", d.f.US.String())
	r := b.do(http.MethodPost, "/journal/new", v)
	status(t, "the company changed", r, http.StatusUnprocessableEntity)
	p := r.page(t)
	holds(t, "the company changed", p.mainText(), "The company was changed", "Check them and send the entry again.", "The company's currency is USD.")
	again := formValues(p.form("/journal/new"))
	if again.Get("currency") != "USD" || again.Get("filled_for") != d.f.US.String() || again.Get("line_amount") != "5.00" {
		t.Errorf("the company changed: got %v, want company 1000's USD, its lines kept", again)
	}
	again.Set("posting_date", "2026-03-15")
	again.Set("document_date", "2026-03-15")
	id := parkedIn(t, b.do(http.MethodPost, "/journal/new", again))
	var currency string
	pgtest.Scan(t, `SELECT currency FROM ledger.parked_entry WHERE id = $1`, []any{id}, &currency)
	if currency != "USD" {
		t.Errorf("parked after the company changed: currency %s, want USD", currency)
	}
	hx, headers := asHTMX(v, ui.OpCompany)
	frag := b.do(http.MethodPost, "/journal/new", hx, headers...)
	status(t, "the company chosen with htmx", frag, http.StatusOK)
	if strings.Contains(frag.body, "<html") || !strings.Contains(frag.body, `id="entry-form"`) {
		t.Errorf("the company chosen with htmx: got %q, want the form alone", frag.body)
	}
	if got := field(frag.page(t).doc, "currency"); got != "USD" {
		t.Errorf("the company chosen with htmx: currency %q, want USD", got)
	}
	forged := withLines(newEntryForm(b, d, nil), pair("1.00")...)
	forged.Set("legal_entity_id", "x")
	holds(t, "a company that is no id", b.do(http.MethodPost, "/journal/new", forged).page(t).mainText(), "Choose the company.")
}

// TestRunningTotals: the form's debits, credits and difference as it is
// rendered, and alone with htmx when an amount changes, in the person's
// locale; a new entry's and a change's.
func TestRunningTotals(t *testing.T) {
	s, d := journalStack(t)
	b := s.as(d.accountant)
	v := withLines(newEntryForm(b, d, nil), typedLine{account: "520000", amount: "1200,5"}, typedLine{account: "530000", amount: "99,50"},
		typedLine{account: "100000", amount: "-1000"}, typedLine{account: "100000", amount: "x"})
	hx, headers := asHTMX(v, ui.OpTotals)
	r := b.do(http.MethodPost, "/journal/new", hx, append(headers, "Accept-Language", "fr-CA")...)
	status(t, "the totals", r, http.StatusOK)
	if strings.Contains(r.body, "<html") || strings.Contains(r.body, `id="entry-totals"`) || !strings.Contains(r.body, "summary-list") {
		t.Errorf("the totals with htmx: got %q, want their contents alone, swapped inside their live region", r.body)
	}
	holds(t, "the totals", text(r.page(t).doc), "Débits 1 300,00 EUR", "Crédits 1 000,00 EUR", "Écart 300,00 EUR",
		"Le montant de 1 ligne ne peut être lu")
	v.Set("op", "add")
	holds(t, "a line added", b.do(http.MethodPost, "/journal/new", v, "Accept-Language", "fr-CA").page(t).mainText(), "Totaux courants",
		"Débits 1 300,00 EUR")
	id := parkAs(b, d, pair("7.25")...)
	edit := formValues(b.get(parkedPath(id) + "/edit").page(t).form(parkedPath(id) + "/edit"))
	hx, headers = asHTMX(edit, ui.OpTotals)
	holds(t, "a change's totals", text(b.do(http.MethodPost, parkedPath(id)+"/edit", hx, headers...).page(t).doc), "Debits EUR 7.25",
		"Difference EUR 0.00")
}

// TestLineTaxAndQuantity: a line's tax jurisdiction, declared tax base
// and quantity are checked before the API is called, sent as the API
// takes them, and refused by it on their line; a parked line's are shown
// to be changed in the person's locale, and its partner by number.
func TestLineTaxAndQuantity(t *testing.T) {
	s, d := journalStack(t)
	b := s.as(d.accountant)
	send := func(lines ...map[string]string) response {
		v := withLines(newEntryForm(b, d, nil), pair("3.00")...)
		for i, l := range lines {
			for k, x := range l {
				v["line_"+k][i] = x
			}
		}
		return b.do(http.MethodPost, "/journal/new", v, "Accept-Language", "fr-CA")
	}
	for name, tc := range map[string]struct {
		line map[string]string
		want string
	}{
		"a jurisdiction":         {map[string]string{"tax_jurisdiction": "ontario"}, "par son code ISO 3166-2"},
		"a tax base":             {map[string]string{"tax_base": "x"}, "Entrez le montant en chiffres"},
		"a quantity":             {map[string]string{"quantity": "1.1234567", "unit": "KG"}, "au plus 6 décimales"},
		"a quantity's unit":      {map[string]string{"quantity": "2"}, "Une quantité exige son unité."},
		"a unit's quantity":      {map[string]string{"unit": "KG"}, "Une unité exige sa quantité."},
		"a unit":                 {map[string]string{"quantity": "2", "unit": "KILOGRAM"}, "de 1 à 6 lettres majuscules"},
		"refused by the API":     {map[string]string{"tax_code": "V0", "tax_jurisdiction": "CA-ON"}, "ledger.jurisdiction_not_levied"},
		"a unit the API refuses": {map[string]string{"quantity": "2", "unit": "ZZZ"}, "ledger.unknown_unit"},
	} {
		t.Run(name, func(t *testing.T) {
			r := send(tc.line)
			if r.status < 400 {
				t.Fatalf("got %d to %s, want a refusal", r.status, r.location())
			}
			holds(t, name, r.page(t).mainText(), tc.want)
		})
	}
	holds(t, "on its line", send(map[string]string{"tax_code": "V0", "tax_jurisdiction": "CA-ON"}).page(t).mainText(),
		"Territoire fiscal : refusé par l'ERP (not_levied).")
	v := withLines(newEntryForm(b, d, nil), typedLine{account: "520000", amount: "3,00"}, typedLine{account: "211000", amount: "-3,00", partner: "BP90000001"})
	v["line_quantity"], v["line_unit"] = []string{"2,5", ""}, []string{"kg", ""}
	id := parkedIn(t, b.do(http.MethodPost, "/journal/new", v, "Accept-Language", "fr-CA"))
	var quantity, unit string
	pgtest.Scan(t, `SELECT quantity::text, uom_code FROM ledger.parked_entry_line WHERE parked_entry_id = $1 AND line_no = 1`, []any{id}, &quantity, &unit)
	if !strings.HasPrefix(quantity, "2.5") || unit != "KG" {
		t.Errorf("the line's quantity: got %s %s, want 2.5 KG", quantity, unit)
	}
	edit := formValues(b.get(parkedPath(id)+"/edit", "Accept-Language", "fr-CA").page(t).form(parkedPath(id) + "/edit"))
	if edit["line_quantity"][0] != "2,5" || edit["line_unit"][0] != "KG" || edit["line_partner"][1] != "BP90000001" {
		t.Errorf("the change's form: got quantities %v %v and partners %v, want 2,5 KG and the partner's number", edit["line_quantity"], edit["line_unit"],
			edit["line_partner"])
	}
	parkedDecided(t, "the change sent back as shown", b.do(http.MethodPost, parkedPath(id)+"/edit", edit, "Accept-Language", "fr-CA"), id, "changed")
}

// parkFileAs posts the new entry's form as fields, with the file when not
// nil, as a multipart form to the file's path, as the form's file button
// sends it.
func parkFileAs(b *browser, fields url.Values, file []byte, header ...string) response {
	b.s.t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, vs := range fields {
		for _, x := range vs {
			_ = w.WriteField(k, x) // into memory
		}
	}
	if file != nil {
		part, _ := w.CreateFormFile("file", "lines.csv") // into memory
		_, _ = part.Write(file)
	}
	_ = w.Close()
	return b.raw(http.MethodPost, ui.NewEntryFile, &body, w.FormDataContentType(), header...)
}

// TestParkFromFile: a new entry parked with a CSV file's lines under the
// form's header, in the person's locale; the file, its rows, the header,
// a company changed or the API's refusal said on the form, the lines
// typed kept.
func TestParkFromFile(t *testing.T) {
	s, d := journalStack(t)
	b := s.as(d.accountant)
	v := newEntryForm(b, d, url.Values{"header_text": {"From a file"}})
	csv := "account;amount;cost_center;quantity;unit\n520000;300,00;CC100;3;EA\n530000;200,00;;;\n100000;-500,00;;;\n"
	id := parkedIn(t, parkFileAs(b, v, []byte(csv), "Accept-Language", "fr-CA"))
	var lines int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.parked_entry_line WHERE parked_entry_id = $1`, []any{id}, &lines)
	if lines != 3 {
		t.Errorf("parked from a file: got %d lines, want the file's 3", lines)
	}
	holds(t, "parked from a file", b.get(parkedPath(id)).page(t).mainText(), "Header text From a file", "530000 IT Services EUR 200.00")
	typed := withLines(newEntryForm(b, d, nil), typedLine{account: "520000", amount: "1.00", text: "Typed"})
	switched := withLines(newEntryForm(b, d, nil), pair("1.00")...)
	switched.Set("legal_entity_id", d.f.US.String())
	wrongHeader := newEntryForm(b, d, url.Values{"currency": {"euro"}})
	for name, tc := range map[string]struct {
		fields url.Values
		file   []byte
		want   []string
	}{
		"no file":            {typed, nil, []string{"Choose the CSV file.", "Choose the file again"}},
		"wrong rows":         {typed, []byte("account,amount\n999999,1.00\n520000,x\n"), []string{"Rows of the file to correct", "Row 3, amount"}},
		"the header wrong":   {wrongHeader, []byte("account,amount\n520000,1.00\n100000,-1.00\n"), []string{"Enter the currency as three capital letters"}},
		"the company":        {switched, []byte("account,amount\n520000,1.00\n100000,-1.00\n"), []string{"The company was changed"}},
		"refused by the API": {typed, []byte("account,amount\n520000,1.00\n100000,-2.00\n"), []string{"ledger.unbalanced"}},
		"a row the API refuses": {typed, []byte("account,amount,cost_center\n520000,1.00,\n100000,-1.00,CC100\n"),
			[]string{"Row 3, cost_center: refused by the ERP (not_allowed)."}},
	} {
		t.Run(name, func(t *testing.T) {
			fields := url.Values{}
			for k, vs := range tc.fields {
				fields[k] = vs
			}
			fields.Set("idempotency_key", uuid.NewString()) // each a form of its own
			r := parkFileAs(b, fields, tc.file)
			if r.status < 400 {
				t.Fatalf("got %d to %s, want a refusal", r.status, r.location())
			}
			p := r.page(t)
			holds(t, name, p.mainText(), tc.want...)
			if got := formValues(p.form("/journal/new"))["line_account"]; len(got) < 2 || got[0] != tc.fields.Get("line_account") {
				t.Errorf("the lines typed: got %v, want them kept", got)
			}
		})
	}
	bad := url.Values{}
	for k, vs := range typed {
		bad[k] = vs
	}
	bad.Set("idempotency_key", "x")
	status(t, "a form that is not the page's", parkFileAs(b, bad, []byte(csv)), http.StatusBadRequest)
}

// TestPostToLedgers: a new entry of company 2000, which keeps the leading
// ledger and a parallel one, is parked in one ledger and posted to a
// ledger group from the form; a choice not offered is refused on its
// field.
func TestPostToLedgers(t *testing.T) {
	var f fixture.Env
	s := newStack(t, stackOptions{permissions: fixture.JournalPermissions, seed: func(e fixture.Env) { f = e }, extend: fixture.Parallel})
	b := s.browser()
	b.signIn()
	p := b.get("/journal/new?legal_entity_id=" + f.DE.String()).page(t)
	holds(t, "the ledgers offered", text(p.all("select", "")[1]), "Every ledger the company keeps", "Ledger 0L Leading ledger", "Ledger 2L HGB",
		"Ledger group ALL Every ledger")
	v := formValues(p.form("/journal/new"))
	v.Set("posting_date", "2026-03-15")
	v.Set("document_date", "2026-03-15")
	v = withLines(v, pair("8.00")...)
	hgb := f.Ledger(t, "2L")
	v.Set("post_to", ui.PostToLedger+hgb.String())
	id := parkedIn(t, b.do(http.MethodPost, "/journal/new", v))
	var ledger uuid.UUID
	pgtest.Scan(t, `SELECT ledger_id FROM ledger.parked_entry WHERE id = $1`, []any{id}, &ledger)
	if ledger != hgb {
		t.Errorf("parked in 2L: got ledger %s, want %s", ledger, hgb)
	}
	v.Set("post_to", ui.PostToGroup+"ALL")
	v.Set("op", "post")
	v.Set("idempotency_key", uuid.NewString())
	posted := postedIn(t, b.do(http.MethodPost, "/journal/new", v))
	var copies int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.journal_entry e JOIN ledger.journal_entry p ON p.tenant_id = e.tenant_id
		AND p.posting_group_id = e.posting_group_id WHERE p.id = $1`, []any{posted}, &copies)
	if copies != 2 {
		t.Errorf("posted to the group ALL: got %d entries, want one in each of its 2 ledgers", copies)
	}
	for _, wrong := range []string{"ledger:x", "group:all", "book:0L"} {
		t.Run(wrong, func(t *testing.T) {
			v.Set("post_to", wrong)
			r := b.do(http.MethodPost, "/journal/new", v)
			status(t, wrong, r, http.StatusUnprocessableEntity)
			p := r.page(t)
			holds(t, wrong, p.mainText(), "Choose the ledgers among those offered.")
			if got := chosen(p.doc, "post_to"); got != wrong {
				t.Errorf("the choice sent again: got %q, want %q kept", got, wrong)
			}
		})
	}
}

// TestPartnersNamedOnEntries is the GL accountant's and the controller's
// reading of an entry (T73h3): a parked and a posted entry's lines name
// their partners by number and name, read with the entry
// (expand=partners), so the pages' calls stay six; a line without a
// partner is unchanged; a person who may not read partners sees the id,
// as before.
func TestPartnersNamedOnEntries(t *testing.T) {
	s, d := journalStack(t)
	b := s.as(d.accountant)
	id := parkAs(b, d, typedLine{account: "520000", amount: "5.00", text: "Freight"}, typedLine{account: "211000", amount: "-5.00", partner: "BP90000001"})
	supplier := d.f.Supplier.String()
	read := func(t *testing.T, who *browser, path string, wants ...string) {
		t.Helper()
		got := who.get(path).page(t).mainText()
		holds(t, path, got, wants...)
		for _, w := range wants {
			if w == supplier {
				return // the id expected: not named
			}
		}
		if strings.Contains(got, supplier) {
			t.Errorf("%s: got the partner's id %s, want it named", path, supplier)
		}
	}
	t.Run("parked", func(t *testing.T) {
		read(t, b, parkedPath(id), "520000 Office Supplies EUR 5.00 None None None None None Freight", "BP90000001 BP90000001")
	})
	parkedDecided(t, "posted", decideParkedAs(b, id, "post", nil), id, "posted")
	var journal uuid.UUID
	pgtest.Scan(t, `SELECT journal_entry_id FROM ledger.parked_entry WHERE id = $1`, []any{id}, &journal)
	t.Run("posted", func(t *testing.T) {
		read(t, b, entryPath(journal), "BP90000001 BP90000001")
	})
	t.Run("posted, in French", func(t *testing.T) {
		got := b.get(entryPath(journal), "Accept-Language", "fr-CA").page(t).mainText()
		holds(t, "fr-CA", got, "BP90000001 BP90000001")
	})
	viewer := s.as(d.viewer)
	t.Run("a person who may not read partners", func(t *testing.T) {
		read(t, viewer, entryPath(journal), supplier)
		read(t, viewer, parkedPath(id), supplier)
	})
}
