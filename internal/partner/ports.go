package partner

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Postings answers the ledger's question whether a partner may be posted
// to, in the request's transaction.
type Postings struct{}

// PostingBlock says why the partner may not be posted on a reconciliation
// account of the kind (supplier or customer) in the legal entity, or ""
// when it may: the partner must be active, play the role and, as a
// supplier, be set up and not blocked for posting in the legal entity.
func (Postings) PostingBlock(ctx context.Context, partnerID, legalEntity uuid.UUID, kind string) (string, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return "", err
	}
	var status string
	var hasRole bool
	var blocked *bool
	err = tx.QueryRow(ctx, `SELECT p.status,
			EXISTS (SELECT FROM partner.partner_role r WHERE r.tenant_id = p.tenant_id AND r.partner_id = p.id AND r.role = $3),
			(SELECT sc.is_posting_blocked FROM partner.supplier_company sc WHERE sc.tenant_id = p.tenant_id AND sc.partner_id = p.id AND sc.legal_entity_id = $2)
		FROM partner.business_partner p WHERE p.id = $1`, partnerID, legalEntity, kind).Scan(&status, &hasRole, &blocked)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "there is no business partner with this id", nil
	case err != nil:
		return "", fmt.Errorf("partner: read the posting status of business partner %s in legal entity %s: %w", partnerID, legalEntity, err)
	case status != Active:
		return "the business partner is " + status, nil
	case !hasRole:
		return "the business partner does not have the " + kind + " role", nil
	case kind == "supplier" && blocked == nil:
		return "the supplier is not set up in this legal entity", nil
	case kind == "supplier" && *blocked:
		return "the supplier is blocked for posting in this legal entity", nil
	}
	return "", nil
}

// PaymentTerms is the configuration document's payment_terms section,
// kept in partner.payment_term.
type PaymentTerms struct{}

// Check refuses removing a payment term: suppliers may use it, and terms
// have no lifecycle to retire them.
func (PaymentTerms) Check(_ context.Context, base, proposed *config.Config) ([]config.Finding, error) {
	if base == nil {
		return nil, nil
	}
	kept := map[string]bool{}
	for _, t := range proposed.PaymentTerms {
		kept[t.Key] = true
	}
	var out []config.Finding
	for _, t := range base.PaymentTerms {
		if !kept[t.Key] {
			out = append(out, config.Finding{Severity: config.SevError, Path: "payment_terms[" + t.Key + "]",
				Message: "a payment term cannot be removed once activated"})
		}
	}
	return out, nil
}

// Apply creates new payment terms and updates changed ones, with their
// cash discount tiers (validated in the document: at most two, later
// tiers smaller, all within the net days).
func (PaymentTerms) Apply(ctx context.Context, doc *config.Config) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	for _, t := range doc.PaymentTerms {
		if len(t.Discounts) > 2 {
			return fmt.Errorf("partner: apply payment term %s: %d discount tiers, the table holds two", t.Key, len(t.Discounts))
		}
		var days [2]*int
		var pct [2]*string
		for i, d := range t.Discounts {
			days[i], pct[i] = &d.Days, &d.Percent
		}
		if _, err := tx.Exec(ctx, `INSERT INTO partner.payment_term AS t (tenant_id, code, description, net_days, discount_days, discount_pct,
				discount2_days, discount2_pct, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6::numeric, $7, $8::numeric, $9, $9)
			ON CONFLICT (tenant_id, code) DO UPDATE SET description = EXCLUDED.description, net_days = EXCLUDED.net_days,
				discount_days = EXCLUDED.discount_days, discount_pct = EXCLUDED.discount_pct, discount2_days = EXCLUDED.discount2_days,
				discount2_pct = EXCLUDED.discount2_pct, version = t.version + 1, updated_by = EXCLUDED.updated_by
			WHERE (t.description, t.net_days, t.discount_days, t.discount_pct, t.discount2_days, t.discount2_pct)
				IS DISTINCT FROM (EXCLUDED.description, EXCLUDED.net_days, EXCLUDED.discount_days, EXCLUDED.discount_pct,
					EXCLUDED.discount2_days, EXCLUDED.discount2_pct)`,
			p.TenantID, t.Key, t.Description, t.NetDays, days[0], pct[0], days[1], pct[1], p.ID); err != nil {
			return fmt.Errorf("partner: apply payment term %s: %w", t.Key, err)
		}
	}
	return nil
}

// Suppliers answers procurement's and payables' questions about suppliers
// in the request's transaction.
type Suppliers struct{}

// Supplier is a business partner as a supplier of one legal entity.
type Supplier struct {
	ID               uuid.UUID
	Number           string
	Name             string
	CountryCode      string
	Status           string // business partner status
	IsSupplier       bool   // plays the supplier role
	SetUp            bool   // has supplier data in the legal entity
	ReconciliationID uuid.UUID
	PaymentTermCode  string
	IsPostingBlocked bool
	IsPaymentBlocked bool
	// WithholdingTaxCodes are the supplier's withholding tax codes in the
	// legal entity (T46e).
	WithholdingTaxCodes []string
	// PaymentMethods are the methods the supplier may be paid by in the
	// legal entity, in order (ADR-0038).
	PaymentMethods []string
	// PlanningGroup is the supplier's planning group in the legal entity,
	// "" for none (ADR-0051 §4).
	PlanningGroup string
	// TaxSchemes are the schemes of the tax identifiers the partner holds
	// (CA_GST, AE_TRN, ...): the registration number a document's input tax
	// recovery may require is one of the company's country (ADR-0049 §6).
	TaxSchemes []string
}

// Usable reports why the supplier cannot be ordered from or invoiced in
// the legal entity, or "" when it can.
func (s Supplier) Usable() string {
	switch {
	case s.Status != Active:
		return "the business partner is " + s.Status
	case !s.IsSupplier:
		return "the business partner is not a supplier"
	case !s.SetUp:
		return "the supplier is not set up in this legal entity"
	case s.IsPostingBlocked:
		return "the supplier is blocked for posting in this legal entity"
	}
	return ""
}

// Supplier returns the partner as a supplier of the legal entity, or
// ok = false when there is no partner with the id.
func (Suppliers) Supplier(ctx context.Context, partnerID, legalEntity uuid.UUID) (Supplier, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Supplier{}, false, err
	}
	s := Supplier{ID: partnerID}
	var term *string
	var recon *uuid.UUID
	var posting, payment *bool
	err = tx.QueryRow(ctx, `SELECT p.number, p.name, p.country_code, p.status,
			EXISTS (SELECT FROM partner.partner_role r WHERE r.tenant_id = p.tenant_id AND r.partner_id = p.id AND r.role = 'supplier'),
			sc.reconciliation_account_id, sc.payment_term_code, sc.is_posting_blocked, sc.is_payment_blocked,
			coalesce((SELECT array_agg(w.code ORDER BY w.code) FROM partner.supplier_withholding w WHERE w.tenant_id = p.tenant_id AND w.partner_id = p.id
				AND w.legal_entity_id = $2 AND w.is_active), '{}'), coalesce(sc.payment_methods, '{}'), coalesce(sc.planning_group, ''),
			coalesce((SELECT array_agg(t.scheme ORDER BY t.scheme) FROM partner.tax_identifier t WHERE t.tenant_id = p.tenant_id AND t.partner_id = p.id), '{}')
		FROM partner.business_partner p
		LEFT JOIN partner.supplier_company sc ON sc.tenant_id = p.tenant_id AND sc.partner_id = p.id AND sc.legal_entity_id = $2
		WHERE p.id = $1`, partnerID, legalEntity).Scan(&s.Number, &s.Name, &s.CountryCode, &s.Status, &s.IsSupplier, &recon, &term, &posting, &payment,
		&s.WithholdingTaxCodes, &s.PaymentMethods, &s.PlanningGroup, &s.TaxSchemes)
	if errors.Is(err, pgx.ErrNoRows) {
		return Supplier{}, false, nil
	}
	if err != nil {
		return Supplier{}, false, fmt.Errorf("partner: read supplier %s of legal entity %s: %w", partnerID, legalEntity, err)
	}
	if term != nil {
		s.SetUp, s.ReconciliationID, s.PaymentTermCode, s.IsPostingBlocked, s.IsPaymentBlocked = true, *recon, *term, *posting, *payment
	}
	return s, true, nil
}

// PaymentAccount is a partner's payments bank account as a migration's
// reconciliation compares it with the source system's: the scheme, the
// bank and the account's last four characters, never the account.
type PaymentAccount struct {
	Scheme, BankCountryCode string
	// InstitutionNumber and TransitNumber are a Canadian account's bank
	// (scheme ca, T45d); "" for the other schemes.
	InstitutionNumber, TransitNumber string
	Last4                            string
}

// PaymentAccounts returns the payments bank account valid today of each
// partner that has one, keyed by partner id (T72d).
func (Suppliers) PaymentAccounts(ctx context.Context, partners []uuid.UUID) (map[uuid.UUID]PaymentAccount, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT partner_id, account_scheme, bank_country_code, coalesce(institution_number, ''), coalesce(transit_number, ''), account_last4
		FROM partner.bank_account WHERE partner_id = ANY($1) AND purpose = 'payments'
			AND valid_from <= current_date AND (valid_to IS NULL OR valid_to > current_date)`, partners)
	out := map[uuid.UUID]PaymentAccount{}
	var id uuid.UUID
	var a PaymentAccount
	_, err = pgx.ForEachRow(rows, []any{&id, &a.Scheme, &a.BankCountryCode, &a.InstitutionNumber, &a.TransitNumber, &a.Last4}, func() error {
		out[id] = a
		return nil
	})
	if err != nil {
		return out, fmt.Errorf("partner: read the payments bank accounts of %d partners: %w", len(partners), err)
	}
	return out, nil
}

// PaymentTerm is a payment term as payables posts with it: its net days
// and up to two cash discount tiers, earliest first (T36a).
type PaymentTerm struct {
	Code string
	// Description is the configured text of the term, for documents sent
	// to partners (T72g).
	Description string
	NetDays     int
	Discounts   []Discount
}

// Discount is a cash discount tier: Percent (a plain decimal) off for
// payment within Days of the baseline date.
type Discount struct {
	Days    int
	Percent string
}

// PaymentTerm returns the configured payment term, or ok = false.
func (Suppliers) PaymentTerm(ctx context.Context, code string) (PaymentTerm, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return PaymentTerm{}, false, err
	}
	t := PaymentTerm{Code: code}
	var days1, days2 *int
	var pct1, pct2 *string
	err = tx.QueryRow(ctx, `SELECT description, net_days, discount_days, trim_scale(discount_pct)::text, discount2_days, trim_scale(discount2_pct)::text
		FROM partner.payment_term WHERE code = $1`, code).Scan(&t.Description, &t.NetDays, &days1, &pct1, &days2, &pct2)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentTerm{}, false, nil
	}
	if err != nil {
		return PaymentTerm{}, false, fmt.Errorf("partner: read payment term %s: %w", code, err)
	}
	if days1 != nil { // a tier's days and percent are null together (000004, 000067)
		t.Discounts = append(t.Discounts, Discount{Days: *days1, Percent: *pct1})
	}
	if days2 != nil {
		t.Discounts = append(t.Discounts, Discount{Days: *days2, Percent: *pct2})
	}
	return t, true, nil
}

// OrderAddress returns the address a purchase order is sent to: the
// partner's order_from address valid today, else its legal one, or
// ok = false when it holds neither (T72g, ADR-0054). Today is the
// database's day, as for bank accounts: the document goes out now.
func (Suppliers) OrderAddress(ctx context.Context, partnerID uuid.UUID) (Address, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Address{}, false, err
	}
	var a Address
	err = tx.QueryRow(ctx, `SELECT id, purpose, line1, line2, city, postal_code, region, country_code, valid_from
		FROM partner.address WHERE partner_id = $1 AND purpose IN ('order_from', 'legal')
			AND valid_from <= current_date AND (valid_to IS NULL OR valid_to > current_date)
		ORDER BY purpose = 'order_from' DESC LIMIT 1`, partnerID).
		Scan(&a.ID, &a.Purpose, &a.Line1, &a.Line2, &a.City, &a.PostalCode, &a.Region, &a.CountryCode, &a.ValidFrom.Time)
	if errors.Is(err, pgx.ErrNoRows) {
		return Address{}, false, nil
	}
	if err != nil {
		return Address{}, false, fmt.Errorf("partner: read the order address of business partner %s: %w", partnerID, err)
	}
	return a, true, nil
}

// PaymentTermExists reports whether the payment terms are configured.
func (Suppliers) PaymentTermExists(ctx context.Context, code string) (bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return false, err
	}
	var ok bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM partner.payment_term WHERE code = $1)`, code).Scan(&ok)
	if err != nil {
		return ok, fmt.Errorf("partner: find payment term %s: %w", code, err)
	}
	return ok, nil
}

// WithholdingCodes is the configuration section of suppliers' withholding
// tax codes (T46e): the codes themselves are the ledger's, kept from the
// document by ledger.Setup. A document that takes away a code a supplier
// has, or that a pending partner change gives one, is refused when it is
// submitted (Check) and again when it is activated (Apply), under the
// tenant's withholding lock, which a partner change giving codes takes too,
// so neither slips past the other.
type WithholdingCodes struct {
	LegalEntities EntitiesByCode
}

// Check reports each company of the proposed document whose suppliers, or
// pending partner changes, have a withholding tax code it does not.
func (w WithholdingCodes) Check(ctx context.Context, base, proposed *config.Config) ([]config.Finding, error) {
	return withholdingCodes.check(ctx, w.LegalEntities, base, proposed)
}

// Apply refuses to activate a document that takes away a code suppliers
// have (409 partner.withholding_tax_code_in_use): the activation rolls back.
func (w WithholdingCodes) Apply(ctx context.Context, doc *config.Config) error {
	return withholdingCodes.apply(ctx, w.LegalEntities, doc)
}

// PaymentMethodCodes is the configuration section of suppliers' payment
// methods (ADR-0038): the methods themselves are the payments context's,
// kept from the document by payments.PaymentMethods. Like
// WithholdingCodes, a document taking away a method a supplier has, or a
// pending partner change gives one, is refused, under the tenant's payment
// method lock.
type PaymentMethodCodes struct {
	LegalEntities EntitiesByCode
}

// Check reports each company of the proposed document whose suppliers, or
// pending partner changes, have a payment method it does not.
func (m PaymentMethodCodes) Check(ctx context.Context, base, proposed *config.Config) ([]config.Finding, error) {
	return paymentMethodCodes.check(ctx, m.LegalEntities, base, proposed)
}

// Apply refuses to activate a document that takes away a method suppliers
// have (409 partner.payment_method_in_use): the activation rolls back.
func (m PaymentMethodCodes) Apply(ctx context.Context, doc *config.Config) error {
	return paymentMethodCodes.apply(ctx, m.LegalEntities, doc)
}

// PlanningGroupCodes is the configuration section of suppliers' planning
// groups (ADR-0051 §4): the groups themselves are the cash context's, kept
// from the document's cash.planning_groups by cash.Setup. Like
// PaymentMethodCodes, a document taking away a group a supplier has, or a
// pending partner change gives one, is refused, under the tenant's planning
// group lock.
type PlanningGroupCodes struct {
	LegalEntities EntitiesByCode
}

// Check reports each planning group the proposed document no longer has
// that suppliers, or pending partner changes, have.
func (g PlanningGroupCodes) Check(ctx context.Context, base, proposed *config.Config) ([]config.Finding, error) {
	return planningGroupCodes.check(ctx, g.LegalEntities, base, proposed)
}

// Apply refuses to activate a document that takes away a group suppliers
// have (409 partner.planning_group_in_use): the activation rolls back.
func (g PlanningGroupCodes) Apply(ctx context.Context, doc *config.Config) error {
	return planningGroupCodes.apply(ctx, g.LegalEntities, doc)
}

// EntitiesByCode finds legal entities by their company code.
type EntitiesByCode interface {
	EntityByCode(ctx context.Context, code string) (tenancy.Entity, bool, error)
}

// supplierCodes is a list of codes in suppliers' company data that each
// company of the configuration defines.
type supplierCodes struct {
	lock  string // the advisory lock a change giving codes and a document taking them away share
	field string // the list in a company of the document and in a change's supplier company
	noun  string // what a code is, for messages
	// inUse selects the codes suppliers have in legal entity $1.
	inUse string
	// kept are the codes the document has for its company.
	kept func(doc *config.Config, co config.Company) []string
	// path is where the document lacks a company's code.
	path func(company, code string) string
}

const (
	withholdingLock   = "partner.withholding."
	paymentMethodLock = "partner.payment_method."
	planningGroupLock = "partner.planning_group."
)

// companyPath is where a code of a company's own list is missing.
func companyPath(field string) func(company, code string) string {
	return func(company, code string) string { return "companies[" + company + "]." + field + "[" + code + "]" }
}

var withholdingCodes = supplierCodes{lock: withholdingLock, field: "withholding_tax_codes", noun: "withholding tax code",
	inUse: `SELECT code FROM partner.supplier_withholding WHERE legal_entity_id = $1 AND is_active`, path: companyPath("withholding_tax_codes"),
	kept: func(_ *config.Config, co config.Company) []string {
		out := make([]string, 0, len(co.WithholdingTaxCodes))
		for _, c := range co.WithholdingTaxCodes {
			out = append(out, c.Code)
		}
		return out
	}}

var paymentMethodCodes = supplierCodes{lock: paymentMethodLock, field: "payment_methods", noun: "payment method",
	inUse: `SELECT unnest(payment_methods) FROM partner.supplier_company WHERE legal_entity_id = $1`, path: companyPath("payment_methods"),
	kept: func(_ *config.Config, co config.Company) []string {
		out := make([]string, 0, len(co.PaymentMethods))
		for _, m := range co.PaymentMethods {
			out = append(out, m.Code)
		}
		return out
	}}

// planningGroupCodes are the tenant's, the same for every company.
var planningGroupCodes = supplierCodes{lock: planningGroupLock, field: "planning_group", noun: "planning group",
	inUse: `SELECT planning_group FROM partner.supplier_company WHERE legal_entity_id = $1 AND planning_group IS NOT NULL`,
	path:  func(_, code string) string { return "cash.planning_groups[" + code + "]" },
	kept: func(doc *config.Config, _ config.Company) []string {
		out := make([]string, 0, len(doc.Cash.PlanningGroups))
		for _, g := range doc.Cash.PlanningGroups {
			out = append(out, g.Code)
		}
		return out
	}}

// check reports each company and code suppliers have, or pending partner
// changes give them, that the proposed document's company does not.
func (c supplierCodes) check(ctx context.Context, entities EntitiesByCode, base, proposed *config.Config) ([]config.Finding, error) {
	if base == nil {
		return nil, nil
	}
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	used, err := c.taken(ctx, tx, entities, proposed)
	if err != nil {
		return nil, err
	}
	var out []config.Finding
	reported := map[string]bool{}
	for _, u := range used {
		path := c.path(u[0], u[1])
		if reported[path] { // a tenant's code, used in several companies
			continue
		}
		reported[path] = true
		out = append(out, config.Finding{Severity: config.SevError, Path: path,
			Message: "suppliers have this " + c.noun + ", or a pending partner change gives it; take it off them first"})
	}
	return out, nil
}

// apply refuses a document that takes away a code suppliers have.
func (c supplierCodes) apply(ctx context.Context, entities EntitiesByCode, doc *config.Config) error {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	used, err := c.taken(ctx, tx, entities, doc)
	if err != nil || len(used) == 0 {
		return err
	}
	return problem.Conflict("partner."+strings.ReplaceAll(c.noun, " ", "_")+"_in_use", "Suppliers of company "+used[0][0]+" have "+c.noun+" "+used[0][1]+
		", or a pending partner change gives it, which the configuration no longer has; take it off them first.")
}

// taken are the company and code pairs suppliers have, or pending partner
// changes give them, that the document's companies do not have, under the
// lock. Only the document's companies are read: suppliers are set up only
// in legal entities of configured companies (checkCompanies), and a
// company is never removed from the document, so for a tenant's codes
// (planning groups) these are every legal entity a supplier can use one in.
func (c supplierCodes) taken(ctx context.Context, tx pgx.Tx, entities EntitiesByCode, doc *config.Config) ([][2]string, error) {
	if err := lockCodes(ctx, tx, c.lock); err != nil {
		return nil, err
	}
	var out [][2]string
	for _, co := range doc.Companies {
		le, ok, err := entities.EntityByCode(ctx, co.Code)
		if err != nil {
			return nil, fmt.Errorf("partner: legal entity of company %s: %w", co.Code, err)
		}
		if !ok {
			continue
		}
		rows, _ := tx.Query(ctx, `SELECT DISTINCT code FROM (`+c.inUse+`
				UNION SELECT jsonb_array_elements_text(CASE jsonb_typeof(sc->$3) WHEN 'array' THEN sc->$3 WHEN 'string' THEN jsonb_build_array(sc->$3)
					ELSE '[]' END)
				FROM partner.partner_change ch, jsonb_array_elements(CASE WHEN jsonb_typeof(ch.payload->'supplier_companies') = 'array'
					THEN ch.payload->'supplier_companies' ELSE '[]' END) sc
				WHERE ch.status = 'pending' AND sc->>'legal_entity_id' = $1::text) x(code)
			WHERE code <> ALL($2) ORDER BY code`, le.ID, c.kept(doc, co), c.field)
		codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, fmt.Errorf("partner: %ss in use in %s: %w", c.noun, co.Code, err)
		}
		for _, code := range codes {
			out = append(out, [2]string{co.Code, code})
		}
	}
	return out, nil
}

// lockCodes takes the tenant's lock of the name until the transaction
// ends: a configuration taking codes away and a partner change giving them
// wait for each other, and then see what the other committed.
func lockCodes(ctx context.Context, tx pgx.Tx, name string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1 || platform.require_tenant()::text, 0))`, name); err != nil {
		return fmt.Errorf("partner: lock %s: %w", name, err)
	}
	return nil
}

// References answers whether the record a custom field refers to exists
// (extensions.References), in the request's transaction. The schema
// compiler refuses cost object fields until cost objects exist (T56), so a
// business partner is the one kind of record referred to.
type References struct{}

// Exists reports whether a business partner, in any status, has the id: a
// reference names a record, whatever it may be used for. Any other kind
// names no record this context holds.
func (References) Exists(ctx context.Context, kind extensions.Kind, id uuid.UUID) (bool, error) {
	if kind != extensions.Partner {
		return false, nil
	}
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return false, err
	}
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM partner.business_partner WHERE id = $1)`, id).Scan(&ok); err != nil {
		return false, fmt.Errorf("partner: business partner %s exists: %w", id, err)
	}
	return ok, nil
}
