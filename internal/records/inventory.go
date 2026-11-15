package records

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The personal data inventory (ADR-0079 §9-§10, T100a): each column of
// the database that holds personal data, declared here with the product
// and checked against the database by its tests, which refuse a column
// of a known personal kind the inventory does not name.

// Kinds of data subject whose rows are counted.
const (
	SubjectBusinessPartner = "business_partner"
	SubjectPrincipal       = "principal"
)

// SubjectKinds are the kinds of data subject, in the order they are listed.
var SubjectKinds = []string{SubjectBusinessPartner, SubjectPrincipal}

// masters are the tables that hold each kind of subject, by id.
var masters = map[string][2]string{SubjectBusinessPartner: {"partner", "business_partner"}, SubjectPrincipal: {"identity", "principal"}}

// Categories of personal data.
const (
	CategoryName           = "name"
	CategoryAddress        = "address"
	CategoryContact        = "contact"
	CategoryBankDetails    = "bank_details"
	CategoryIdentifier     = "identifier"
	CategoryNetworkAddress = "network_address"
	CategoryFreeText       = "free_text"
)

// Entry is a column that holds personal data: why it is kept, and, per
// kind of data subject, the column of its table that names the subject,
// by which the subject's rows are counted.
type Entry struct {
	Schema, Table, Column string
	Category, Purpose     string
	Subjects              map[string]string
}

// partner, principal are an entry's link to a business partner or a
// principal by the column naming it.
func partner(column string) map[string]string {
	return map[string]string{SubjectBusinessPartner: column}
}
func principal(column string) map[string]string { return map[string]string{SubjectPrincipal: column} }

// Inventory is the personal data inventory, by schema, table and column.
var Inventory = []Entry{
	{"audit", "audit_event", "client_address", CategoryNetworkAddress, "The network address a request came from, kept with the audit record of each change it made (ADR-0011).", principal("actor_id")},
	{"closing", "year_sign_off", "signed_by_name", CategoryName, "The name of the person who signed a year close off, kept with the sign-off as signed.", principal("signed_by")},
	{"identity", "principal", "display_name", CategoryName, "A person's or agent's name, shown wherever they acted.", principal("id")},
	{"identity", "principal", "email", CategoryContact, "A person's e-mail address, for notifications.", principal("id")},
	{"identity", "principal", "external_subject", CategoryIdentifier, "A person's identity at the tenant's identity provider, by which they sign in.", principal("id")},
	{"integration", "output_message", "recipient", CategoryContact, "The address an output message (a purchase order to a supplier) was sent to.", nil},
	{"integration", "output_message", "subject", CategoryFreeText, "The subject line of an output message, which may name its recipient.", nil},
	{"partner", "address", "city", CategoryAddress, "A business partner's address, printed on its documents.", partner("partner_id")},
	{"partner", "address", "line1", CategoryAddress, "A business partner's address, printed on its documents.", partner("partner_id")},
	{"partner", "address", "line2", CategoryAddress, "A business partner's address, printed on its documents.", partner("partner_id")},
	{"partner", "address", "postal_code", CategoryAddress, "A business partner's address, printed on its documents.", partner("partner_id")},
	{"partner", "address", "region", CategoryAddress, "A business partner's address, printed on its documents.", partner("partner_id")},
	{"partner", "bank_account", "account_cipher", CategoryBankDetails, "A business partner's bank account, encrypted, which payments are made to.", partner("partner_id")},
	{"partner", "bank_account", "account_fingerprint", CategoryBankDetails, "A keyed hash of the account, by which a bank account used by two partners is found.", partner("partner_id")},
	{"partner", "bank_account", "account_last4", CategoryBankDetails, "The account's last four characters, shown instead of the account.", partner("partner_id")},
	{"partner", "bank_account", "bic", CategoryBankDetails, "The bank's identifier of a business partner's account.", partner("partner_id")},
	{"partner", "bank_account", "holder_name", CategoryName, "The name of the account's holder, sent with payments.", partner("partner_id")},
	{"partner", "bank_account", "institution_number", CategoryBankDetails, "The Canadian institution number of a business partner's account.", partner("partner_id")},
	{"partner", "bank_account", "transit_number", CategoryBankDetails, "The Canadian transit number of a business partner's account.", partner("partner_id")},
	{"partner", "business_partner", "legal_name", CategoryName, "A business partner's legal name.", partner("id")},
	{"partner", "business_partner", "name", CategoryName, "A business partner's name, shown and printed on its documents and payments.", partner("id")},
	{"partner", "business_partner", "name_norm", CategoryName, "The name normalised for searching.", partner("id")},
	{"partner", "partner_change", "payload", CategoryFreeText, "A proposed change of a business partner's name, address or bank details, kept with its approval.", partner("partner_id")},
	{"partner", "partner_change", "possible_duplicates", CategoryName, "The numbers and names of the existing partners a new one may duplicate, as they were when it was requested, kept with its approval (T89a); a partner named there is not the change's own.", nil},
	{"partner", "tax_identifier", "value_cipher", CategoryIdentifier, "A business partner's tax identifier, encrypted (a person's social insurance or security number when the partner is a sole trader).", partner("partner_id")},
	{"partner", "tax_identifier", "value_fingerprint", CategoryIdentifier, "A keyed hash of the tax identifier, by which it is found without decrypting it.", partner("partner_id")},
	{"partner", "tax_identifier", "value_last4", CategoryIdentifier, "The tax identifier's last four characters, shown instead of it.", partner("partner_id")},
	{"payments", "bank_statement_line", "details", CategoryBankDetails, "The transactions a bank statement line holds as the bank gave them, with the counterparty's name and account.", nil},
	{"payments", "bank_statement_line", "text", CategoryFreeText, "The bank's text of a statement line, which may name the counterparty.", nil},
	{"sales", "sales_order", "credit_released_by_name", CategoryName, "The name of the person who released an order's credit block, kept as released.", principal("credit_released_by")},
}

// SchemasWithout are the schemas that hold no column of the inventory,
// and why: every other schema of the database has an entry.
var SchemasWithout = map[string]string{
	"agents":        "agent sessions, tool calls and recommendations: references to documents and principals by id",
	"analytics":     "governed metrics and the extract's state: no person's data",
	"assets":        "fixed assets and their postings",
	"cash":          "the liquidity forecast, its snapshots and exports: amounts by liquidity item, no partner's name",
	"collections":   "dunning runs and notices: partners by id",
	"config":        "the configuration document and its change requests",
	"consolidation": "group reporting runs: companies and accounts",
	"controlling":   "cost centers, internal orders and allocations",
	"credit":        "credit limits: partners by id",
	"einvoicing":    "received e-invoices: their bytes are kept in object storage, the rows name the receiver's participant identifier, a company's",
	"grc":           "controls and risks: owners by id",
	"leases":        "lease contracts and their schedules: the lessor, a supplier, by id",
	"ledger":        "the journal and the chart of accounts: partners by id",
	"payables":      "supplier invoices: suppliers by id",
	"platform":      "operations, idempotency keys and the outbox's machinery",
	"procurement":   "requisitions, orders and receipts: suppliers by id",
	"receivables":   "customer invoices and receipts: customers by id",
	"records":       "archiving runs: requesters by id",
	"revenue":       "revenue contracts: customers by id",
	"tax":           "tax returns: the company's registrations",
	"tenancy":       "the tenant and its companies: organisations, not persons",
	"workflow":      "approval requests and decisions: principals by id",
}

// GetPersonalDataInventory implements GET /v1/personal-data-inventory.
func (e *Endpoints) GetPersonalDataInventory(ctx context.Context) (gen.GetPersonalDataInventoryRes, error) {
	if _, _, err := reqctx.Request(ctx); err != nil {
		return nil, err
	}
	out := &gen.PersonalDataInventory{Count: int32(len(Inventory)), Items: make([]gen.PersonalDataEntry, len(Inventory))} // #nosec G115 -- a declared list
	for i, x := range Inventory {
		kinds := []string{}
		for _, k := range SubjectKinds {
			if _, ok := x.Subjects[k]; ok {
				kinds = append(kinds, k)
			}
		}
		out.Items[i] = gen.PersonalDataEntry{Schema: x.Schema, Table: x.Table, Column: x.Column, Category: x.Category, Purpose: x.Purpose, SubjectKinds: kinds}
	}
	return out, nil
}

// Count is how many rows of an inventory entry name a subject and hold a
// value in its column.
type Count struct {
	Entry Entry
	Rows  int64
}

// subjectTable is a table whose rows name a kind of subject by a column:
// its entries are counted by one statement.
type subjectTable struct {
	schema, table, link string
	entries             []Entry
}

// tablesOf are the tables whose entries name the kind of subject, in the
// inventory's order.
func tablesOf(kind string) []*subjectTable {
	var out []*subjectTable
	for _, x := range Inventory {
		link, ok := x.Subjects[kind]
		if !ok {
			continue
		}
		if n := len(out); n > 0 && out[n-1].schema == x.Schema && out[n-1].table == x.Table && out[n-1].link == link {
			out[n-1].entries = append(out[n-1].entries, x)
			continue
		}
		out = append(out, &subjectTable{schema: x.Schema, table: x.Table, link: link, entries: []Entry{x}})
	}
	return out
}

// Subject counts, per inventory entry naming the kind of subject, the
// rows that name the subject and hold a value in the entry's column
// (ADR-0079 §10). It reads no value. The counts are the tenant's, every
// company's, so the caller needs access to all of them; a subject the
// tenant does not have is not found.
func (e *Endpoints) Subject(ctx context.Context, kind string, id uuid.UUID) ([]Count, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(SubjectKinds, kind) {
		list := strings.Join(SubjectKinds, ", ")
		return nil, problem.Unprocessable("request.validation_failed", "The request is not valid: kind is one of "+list+".",
			problem.FieldError{Field: "kind", Code: "invalid", Message: "one of " + list})
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("records: company access for the personal data of %s %s: %w", kind, id, err)
	}
	if !access.All {
		return nil, problem.Forbidden("records.all_companies_required",
			"Where a data subject's data is held is counted over every company of the tenant; the caller needs access to all companies.")
	}
	master := masters[kind]
	var found bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+pgx.Identifier{master[0], master[1]}.Sanitize()+" WHERE id = $1)", id).Scan(&found); err != nil {
		return nil, fmt.Errorf("records: is there %s %s: %w", kind, id, err)
	}
	if !found {
		return nil, problem.NotFoundOf(strings.ReplaceAll(kind, "_", " "))
	}
	var out []Count
	for _, t := range tablesOf(kind) {
		counts, err := t.count(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, counts...)
	}
	return out, nil
}

// count counts the table's rows that name the subject, per entry those
// holding a value in its column, in one statement. The identifiers are
// the inventory's, sanitised; the subject is a parameter.
func (t *subjectTable) count(ctx context.Context, tx pgx.Tx, id uuid.UUID) ([]Count, error) {
	cols := make([]string, len(t.entries))
	for i, x := range t.entries {
		cols[i] = "count(*) FILTER (WHERE " + pgx.Identifier{x.Column}.Sanitize() + " IS NOT NULL)"
	}
	sql := "SELECT " + strings.Join(cols, ", ") + " FROM " + pgx.Identifier{t.schema, t.table}.Sanitize() + " WHERE " + pgx.Identifier{t.link}.Sanitize() + " = $1"
	rows := make([]int64, len(t.entries))
	dest := make([]any, len(rows))
	for i := range rows {
		dest[i] = &rows[i]
	}
	if err := tx.QueryRow(ctx, sql, id).Scan(dest...); err != nil {
		return nil, fmt.Errorf("records: count the rows of %s.%s naming %s: %w", t.schema, t.table, id, err)
	}
	out := make([]Count, len(rows))
	for i, n := range rows {
		out[i] = Count{Entry: t.entries[i], Rows: n}
	}
	return out, nil
}

// GetPersonalDataSubject implements GET
// /v1/personal-data-inventory/subjects/{kind}/{id}.
func (e *Endpoints) GetPersonalDataSubject(ctx context.Context, params gen.GetPersonalDataSubjectParams) (gen.GetPersonalDataSubjectRes, error) {
	counts, err := e.Subject(ctx, params.Kind, params.ID)
	if err != nil {
		return nil, err
	}
	out := &gen.PersonalDataSubject{SubjectKind: params.Kind, SubjectID: params.ID, Items: make([]gen.PersonalDataSubjectEntry, len(counts))}
	for i, c := range counts {
		out.Items[i] = gen.PersonalDataSubjectEntry{Schema: c.Entry.Schema, Table: c.Entry.Table, Column: c.Entry.Column, Category: c.Entry.Category, Rows: c.Rows}
		out.TotalRows += c.Rows
	}
	return out, nil
}
