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
				AND w.legal_entity_id = $2 AND w.is_active), '{}'), coalesce(sc.payment_methods, '{}')
		FROM partner.business_partner p
		LEFT JOIN partner.supplier_company sc ON sc.tenant_id = p.tenant_id AND sc.partner_id = p.id AND sc.legal_entity_id = $2
		WHERE p.id = $1`, partnerID, legalEntity).Scan(&s.Number, &s.Name, &s.CountryCode, &s.Status, &s.IsSupplier, &recon, &term, &posting, &payment,
		&s.WithholdingTaxCodes, &s.PaymentMethods)
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

// PaymentTerm is a payment term as payables posts with it: its net days
// and up to two cash discount tiers, earliest first (T36a).
type PaymentTerm struct {
	Code      string
	NetDays   int
	Discounts []Discount
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
	err = tx.QueryRow(ctx, `SELECT net_days, discount_days, trim_scale(discount_pct)::text, discount2_days, trim_scale(discount2_pct)::text
		FROM partner.payment_term WHERE code = $1`, code).Scan(&t.NetDays, &days1, &pct1, &days2, &pct2)
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
	// kept are the codes the document's company has.
	kept func(co config.Company) []string
}

const (
	withholdingLock   = "partner.withholding."
	paymentMethodLock = "partner.payment_method."
)

var withholdingCodes = supplierCodes{lock: withholdingLock, field: "withholding_tax_codes", noun: "withholding tax code",
	inUse: `SELECT code FROM partner.supplier_withholding WHERE legal_entity_id = $1 AND is_active`,
	kept: func(co config.Company) []string {
		out := make([]string, 0, len(co.WithholdingTaxCodes))
		for _, c := range co.WithholdingTaxCodes {
			out = append(out, c.Code)
		}
		return out
	}}

var paymentMethodCodes = supplierCodes{lock: paymentMethodLock, field: "payment_methods", noun: "payment method",
	inUse: `SELECT unnest(payment_methods) FROM partner.supplier_company WHERE legal_entity_id = $1`,
	kept: func(co config.Company) []string {
		out := make([]string, 0, len(co.PaymentMethods))
		for _, m := range co.PaymentMethods {
			out = append(out, m.Code)
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
	for _, u := range used {
		out = append(out, config.Finding{Severity: config.SevError, Path: "companies[" + u[0] + "]." + c.field + "[" + u[1] + "]",
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
// lock.
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
				UNION SELECT jsonb_array_elements_text(CASE WHEN jsonb_typeof(sc->$3) = 'array' THEN sc->$3 ELSE '[]' END)
				FROM partner.partner_change ch, jsonb_array_elements(CASE WHEN jsonb_typeof(ch.payload->'supplier_companies') = 'array'
					THEN ch.payload->'supplier_companies' ELSE '[]' END) sc
				WHERE ch.status = 'pending' AND sc->>'legal_entity_id' = $1::text) x(code)
			WHERE code <> ALL($2) ORDER BY code`, le.ID, c.kept(co), c.field)
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
