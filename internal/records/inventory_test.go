package records_test

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/records"
)

// knownPersonal are the known personal kinds of column (ADR-0079 §9):
// names, addresses and contacts, bank details, subjects and client
// addresses, by the column's name. Every column of the database whose
// name is of one of them is named by the inventory or set aside below.
var knownPersonal = regexp.MustCompile(`((^|_)name$|^name_norm$|^line[12]$|^city$|^postal_code$|^region$|email|^recipient$|client_address|^bic$|iban|` +
	`_cipher$|_fingerprint$|_last4$|^holder_name$|^institution_number$|^transit_number$|subject$|phone)`)

// notPersonal are the columns of a known personal kind that hold no
// person's data, each with why.
var notPersonal = map[string]string{
	"analytics.metric_watcher.name":                           "a watcher's name, a label its creator gives the metric it watches",
	"audit.evidence_export.with_client_addresses":             "whether an evidence export's audit events carry their client addresses: a flag, not an address",
	"cash.export.file_name":                                   "a file's name",
	"payments.statement_fetch.file_name":                      "a file's name",
	"cash.liquidity_item.name":                                "a liquidity item's name",
	"cash.planning_group.name":                                "a planning group's name",
	"cash.snapshot.name":                                      "a snapshot's name",
	"closing.accrual_proposal.account_name":                   "an account's name",
	"closing.accrual_proposal.accrual_account_name":           "an account's name",
	"closing.accrual_proposal.cost_center_name":               "a cost center's name",
	"closing.accrual_proposal.profit_center_name":             "a profit center's name",
	"closing.close_task.name":                                 "a close task's name",
	"closing.variance_comment.account_name":                   "an account's name",
	"closing.year_close_task.name":                            "a close task's name",
	"config.regression_suite.name":                            "a regression suite's name",
	"controlling.allocation_cycle.name":                       "an allocation cycle's name",
	"controlling.internal_order.name":                         "an internal order's name",
	"controlling.plan_category.name":                          "a plan category's name",
	"identity.role.name":                                      "a role's name",
	"integration.connector.name":                              "a connector's name",
	"integration.webhook_delivery.subject":                    "an event's subject: the id of the resource it is about",
	"ledger.chart_of_accounts.name":                           "a chart of accounts' name",
	"ledger.cost_center.name":                                 "a cost center's name",
	"ledger.cost_center_category.name":                        "a cost center category's name",
	"ledger.entry_template.name":                              "a template's name",
	"ledger.fiscal_calendar.name":                             "a fiscal calendar's name",
	"ledger.gl_account.name":                                  "an account's name",
	"ledger.hierarchy.name":                                   "a hierarchy's name",
	"ledger.hierarchy_node.name":                              "a hierarchy node's name",
	"ledger.ledger.name":                                      "a ledger's name",
	"ledger.ledger_group.name":                                "a ledger group's name",
	"ledger.profit_center.name":                               "a profit center's name",
	"ledger.segment.name":                                     "a segment's name",
	"payments.house_bank.bic":                                 "the company's own bank account",
	"payments.house_bank.iban_cipher":                         "the company's own bank account",
	"payments.house_bank.iban_last4":                          "the company's own bank account",
	"payments.house_bank.institution_number":                  "the company's own bank account",
	"payments.house_bank.name":                                "the company's own bank account",
	"payments.house_bank.transit_number":                      "the company's own bank account",
	"procurement.gr_ir_clearing.offset_account_name":          "an account's name",
	"procurement.gr_ir_clearing_item.offset_account_name":     "an account's name",
	"tax.registration.number_cipher":                          "the company's own tax registration",
	"tax.registration.number_fingerprint":                     "the company's own tax registration",
	"tax.registration.number_last4":                           "the company's own tax registration",
	"integration.webhook_subscription.secret_cipher":          "a subscription's signing secret",
	"integration.webhook_subscription.previous_secret_cipher": "a subscription's signing secret",
	"tax.tax_return.return_name":                              "a tax return's name",
	"tenancy.legal_entity.name":                               "a company's name",
	"tenancy.org_unit.name":                                   "an organisational unit's name",
	"tenancy.tenant.name":                                     "the tenant's name: an organisation",
	"tenancy.tenant.region":                                   "the cloud region the tenant runs in",
	"public.erp_schema_migration.name":                        "a migration's name",
	"ref.country.iban_length":                                 "reference data",
	"ref.country.name":                                        "reference data",
	"ref.currency.name":                                       "reference data",
	"ref.unit_of_measure.name":                                "reference data",
}

// columns are the database's columns of its tables (not partitions) as
// schema.table.column.
func columns(t *testing.T) []string {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }() // the reads are done; closing loses nothing
	rows, _ := c.Query(ctx, `SELECT n.nspname || '.' || k.relname || '.' || a.attname FROM pg_attribute a
		JOIN pg_class k ON k.oid = a.attrelid JOIN pg_namespace n ON n.oid = k.relnamespace
		WHERE k.relkind IN ('r', 'p') AND NOT k.relispartition AND a.attnum > 0 AND NOT a.attisdropped
			AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema' ORDER BY 1`)
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestInventoryCoversTheDatabase: every entry is a column of the
// database, and so is every link to a subject; every column of a known
// personal kind is named by the inventory or set aside with a reason, so
// a migration adding one fails here until it is; every schema has an
// entry or says why it holds none; the entries are in order, once each
// (ADR-0079 §9, T100a acceptance).
func TestInventoryCoversTheDatabase(t *testing.T) {
	all := columns(t)
	named := map[string]bool{}
	schemas := map[string]bool{}
	for i, x := range records.Inventory {
		col := x.Schema + "." + x.Table + "." + x.Column
		if !slices.Contains(all, col) {
			t.Errorf("entry %s: no such column", col)
		}
		for kind, link := range x.Subjects {
			if !slices.Contains(records.SubjectKinds, kind) || !slices.Contains(all, x.Schema+"."+x.Table+"."+link) {
				t.Errorf("entry %s: link %s by %s is not a kind and a column", col, kind, link)
			}
		}
		if i > 0 {
			prev := records.Inventory[i-1]
			if p := prev.Schema + "." + prev.Table + "." + prev.Column; p >= col {
				t.Errorf("entry %s: after %s, want the inventory by schema, table and column", col, p)
			}
		}
		named[col], schemas[x.Schema] = true, true
	}
	for _, col := range all {
		name := col[strings.LastIndex(col, ".")+1:]
		if knownPersonal.MatchString(name) && !named[col] && notPersonal[col] == "" {
			t.Errorf("column %s is of a known personal kind: name it in records.Inventory, or set it aside in notPersonal with why", col)
		}
	}
	for col := range notPersonal {
		if !slices.Contains(all, col) {
			t.Errorf("set aside %s: no such column", col)
		}
	}
	for _, col := range all {
		s, _, _ := strings.Cut(col, ".")
		if s == "public" || s == "ref" {
			continue // the migrations' own table and reference data
		}
		if _, why := records.SchemasWithout[s]; !schemas[s] && !why {
			t.Errorf("schema %s: no entry; give one or say in records.SchemasWithout why it holds none", s)
		}
		if _, why := records.SchemasWithout[s]; schemas[s] && why {
			t.Errorf("schema %s has entries and is said to hold none", s)
		}
	}
}

// TestPersonalDataInventory: the inventory lists a partner's name and
// bank details and the audit trail's client addresses, with the kinds of
// subject each can be counted for.
func TestPersonalDataInventory(t *testing.T) {
	e := setup(t, nil, "records.privacy.read")
	got, _ := e.Must(200, "GET", "/v1/personal-data-inventory", "", "")
	byColumn := map[string]string{}
	for _, it := range got["items"].([]any) {
		x := it.(map[string]any)
		byColumn[fmt.Sprint(x["schema"], ".", x["table"], ".", x["column"])] = fmt.Sprint(x["category"], " ", x["subject_kinds"])
	}
	for col, want := range map[string]string{
		"partner.business_partner.name":       "name [business_partner]",
		"partner.bank_account.account_cipher": "bank_details [business_partner]",
		"audit.audit_event.client_address":    "network_address [principal]",
		"payments.bank_statement_line.text":   "free_text []",
	} {
		if byColumn[col] != want {
			t.Errorf("%s: got %q, want %q", col, byColumn[col], want)
		}
	}
	if got["count"] != float64(len(records.Inventory)) {
		t.Errorf("count: got %v, want %d", got["count"], len(records.Inventory))
	}
}

// subjectRows are a subject read's counts as "table.column rows", those
// above zero.
func subjectRows(got map[string]any) string {
	var out []string
	for _, it := range got["items"].([]any) {
		x := it.(map[string]any)
		if x["rows"] != float64(0) {
			out = append(out, fmt.Sprint(x["table"], ".", x["column"], " ", x["rows"]))
		}
	}
	return strings.Join(out, ", ")
}

// TestPersonalDataSubject: for a business partner and a principal, each
// entry naming that kind is counted, zero included, and nothing else is
// returned; a subject the tenant does not have is not found, a kind not
// listed refused, and a caller without every company refused (T100a
// acceptance).
func TestPersonalDataSubject(t *testing.T) {
	e := setup(t, nil, "records.privacy.read")
	pgtest.Exec(t, `INSERT INTO partner.address (tenant_id, id, partner_id, purpose, line1, city, postal_code, country_code, valid_from, created_by)
		VALUES ($1, gen_random_uuid(), $2, 'remit_to', '1 Main St', 'Toronto', 'M5V 1A1', 'CA', '2026-01-01', $3)`, e.Tenant.ID, e.Supplier, e.Tenant.Human.ID)
	got, _ := e.Must(200, "GET", "/v1/personal-data-inventory/subjects/business_partner/"+e.Supplier.String(), "", "")
	if s := subjectRows(got); !strings.Contains(s, "business_partner.name 1") || !strings.Contains(s, "address.line1 1") || strings.Contains(s, "address.line2") {
		t.Errorf("the supplier: got %s, want its name and its address's first line", s)
	}
	if n := len(got["items"].([]any)); n != entriesOf(records.SubjectBusinessPartner) {
		t.Errorf("entries: got %d, want every entry naming a business partner, %d", n, entriesOf(records.SubjectBusinessPartner))
	}
	for _, it := range got["items"].([]any) {
		if keys := strings.Join(keysOf(it.(map[string]any)), " "); keys != "category column rows schema table" {
			t.Errorf("an entry's fields: got %s, want no value of the subject's", keys)
		}
	}
	me, _ := e.Must(200, "GET", "/v1/personal-data-inventory/subjects/principal/"+e.Tenant.Human.ID.String(), "", "")
	if s := subjectRows(me); !strings.Contains(s, "principal.display_name 1") {
		t.Errorf("the person: got %s, want their name", s)
	}
	if me["total_rows"].(float64) < 1 {
		t.Errorf("total: got %v", me["total_rows"])
	}
	e.Problem(404, "request.not_found", "GET", "/v1/personal-data-inventory/subjects/business_partner/"+uuid.Must(uuid.NewV7()).String(), "", "")
	e.Problem(404, "request.not_found", "GET", "/v1/personal-data-inventory/subjects/principal/"+e.Supplier.String(), "", "")
	e.Problem(422, "request.validation_failed", "GET", "/v1/personal-data-inventory/subjects/employee/"+e.Supplier.String(), "", "")
	reader := e.Reader(t, "Rita Reader", e.US)
	e.As(reader).Problem(403, "records.all_companies_required", "GET", "/v1/personal-data-inventory/subjects/principal/"+reader.ID.String(), "", "")
}

// entriesOf is how many entries name the kind of subject.
func entriesOf(kind string) int {
	n := 0
	for _, x := range records.Inventory {
		if _, ok := x.Subjects[kind]; ok {
			n++
		}
	}
	return n
}

func keysOf(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestPrivacyDatabaseFailures calls the reads outside a unit of work and
// with each of their database statements failing in turn.
func TestPrivacyDatabaseFailures(t *testing.T) {
	e := setup(t, nil, "records.privacy.read")
	ep := at("2030-01-01")
	for name, call := range map[string]func(ctx context.Context) error{
		"the inventory": func(ctx context.Context) error { _, err := ep.GetPersonalDataInventory(ctx); return err },
		"a subject": func(ctx context.Context) error {
			_, err := ep.GetPersonalDataSubject(ctx, gen.GetPersonalDataSubjectParams{Kind: records.SubjectBusinessPartner, ID: e.Supplier})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error")
			}
		})
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "a subject", func(ctx context.Context) error {
		_, err := ep.GetPersonalDataSubject(ctx, gen.GetPersonalDataSubjectParams{Kind: records.SubjectPrincipal, ID: e.Tenant.Human.ID})
		return err
	})
}

// TestPersonalDataSubjectBudget holds the subject read to the budget of
// a report read (p95 3 s; plan-breadth-week3): a principal who made
// 200,000 audited changes and a tenant of 5,000 partners. No requirement names an audit trail's size
// per person; 200,000 changes is a busy clerk's several years. The audit
// trail is counted by actor without an index of its own (ADR-0079 §10),
// so its rows are the read's cost.
func TestPersonalDataSubjectBudget(t *testing.T) {
	nfr.Measure(t)
	e := setup(t, nil, "records.privacy.read")
	pgtest.Exec(t, `INSERT INTO audit.audit_event (tenant_id, seq, actor_id, actor_kind, action, object_type, object_id, outcome, hash, client_address)
		SELECT $1::uuid, 1000000 + g, $2::uuid, 'human', 'test.thing.do', 'test.thing', gen_random_uuid(), 'succeeded', sha256(g::text::bytea), '10.0.0.1'
		FROM generate_series(1, 200000) g`, e.Tenant.ID, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
		SELECT $1, gen_random_uuid(), 'B' || lpad(n::text, 7, '0'), 'organization', 'Partner ' || n, 'CA', 'active', $2, $2
		FROM generate_series(1, 5000) n`, e.Tenant.ID, e.Tenant.Human.ID)
	pgtest.Exec(t, `ANALYZE audit.audit_event`)
	pgtest.Exec(t, `ANALYZE partner.business_partner`)
	for _, c := range []struct{ name, path string }{
		{"a principal", "principal/" + e.Tenant.Human.ID.String()},
		{"a business partner", "business_partner/" + e.Supplier.String()},
	} {
		t.Run(c.name, func(t *testing.T) {
			var times []time.Duration
			for range 20 {
				start := time.Now()
				e.In(t).Must(200, "GET", "/v1/personal-data-inventory/subjects/"+c.path, "", "")
				times = append(times, time.Since(start))
			}
			slices.Sort(times)
			p95 := times[len(times)*95/100]
			t.Logf("%s: median %v, p95 %v", c.name, times[len(times)/2], p95)
			if budget := nfr.Budget(3 * time.Second); p95 > budget {
				t.Fatalf("p95 %v exceeds the %v budget (3 s scaled by ERP_BUDGET_SCALE)", p95, budget)
			}
		})
	}
}
