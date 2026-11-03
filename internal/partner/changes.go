package partner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/bankfile"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/cabank"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/mergepatch"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

var (
	kinds           = []string{"organization", "person"}
	roles           = []string{"supplier", "customer"}
	addressPurposes = []string{"legal", "remit_to", "order_from", "ship_from"}
	bankPurposes    = []string{"payments", "collections"}
	accountSchemes  = []string{"iban", "domestic", "ca"}
	accountNumber   = regexp.MustCompile(`^[A-Z0-9]{4,34}$`)
)

// SupplierCompanyInput is supplier data for one legal entity.
type SupplierCompanyInput struct {
	LegalEntityID           uuid.UUID `json:"legal_entity_id"`
	ReconciliationAccountID uuid.UUID `json:"reconciliation_account_id"`
	PaymentTermCode         string    `json:"payment_term_code"`
	// WithholdingTaxCodes are the withholding taxes deducted from the
	// supplier in the legal entity (SAP LFBW; T46e), codes of its company.
	WithholdingTaxCodes []string `json:"withholding_tax_codes,omitempty"`
	// PaymentMethods are the methods the supplier may be paid by in the
	// legal entity, in order (SAP LFB1-ZWELS; ADR-0038), methods of its
	// company.
	PaymentMethods []string `json:"payment_methods,omitempty"`
	// PlanningGroup is the supplier's planning group in the legal entity
	// (SAP LFB1-FDGRV; ADR-0051 §4), a code of the configuration's
	// cash.planning_groups; empty for none.
	PlanningGroup string `json:"planning_group,omitempty"`
}

// AddressInput is an address to add.
type AddressInput struct {
	Purpose     string  `json:"purpose"`
	Line1       string  `json:"line1"`
	Line2       *string `json:"line2,omitempty"`
	City        string  `json:"city"`
	PostalCode  *string `json:"postal_code,omitempty"`
	Region      *string `json:"region,omitempty"`
	CountryCode string  `json:"country_code"`
	ValidFrom   Date    `json:"valid_from"`
	ValidTo     *Date   `json:"valid_to,omitempty"`
}

// TaxInput is a tax identifier in clear text.
type TaxInput struct {
	Scheme, Value string
}

// BankInput is a bank account in clear text.
type BankInput struct {
	Purpose         string
	AccountNumber   string
	AccountScheme   string
	BIC             *string
	BankCountryCode string
	HolderName      string
	ValidFrom       time.Time
	// InstitutionNumber and TransitNumber are a Canadian account's bank
	// (scheme ca, T45d), whose account number AccountNumber is.
	InstitutionNumber string
	TransitNumber     string
}

// sealedTax is a tax identifier as a change payload holds it.
type sealedTax struct {
	Scheme           string `json:"scheme"`
	ValueCipher      []byte `json:"value_cipher"`
	ValueFingerprint string `json:"value_fingerprint"`
	ValueLast4       string `json:"value_last4"`
}

// sealedBank is a bank account as a change payload holds it.
type sealedBank struct {
	Purpose            string  `json:"purpose"`
	AccountCipher      []byte  `json:"account_cipher"`
	AccountFingerprint string  `json:"account_fingerprint"`
	AccountLast4       string  `json:"account_last4"`
	AccountScheme      string  `json:"account_scheme"`
	BIC                *string `json:"bic"`
	BankCountryCode    string  `json:"bank_country_code"`
	HolderName         string  `json:"holder_name"`
	ValidFrom          Date    `json:"valid_from"`
	// Absent from the payloads of changes requested before T45d.
	InstitutionNumber *string `json:"institution_number,omitempty"`
	TransitNumber     *string `json:"transit_number,omitempty"`
}

// nullable is a merge-patch value: set, possibly to null.
type nullable struct {
	Set   bool
	Value *string
}

func (n *nullable) UnmarshalJSON(b []byte) error {
	n.Set = true
	if err := json.Unmarshal(b, &n.Value); err != nil {
		return fmt.Errorf("partner: decode a nullable string: %w", err)
	}
	return nil
}

func (n nullable) MarshalJSON() ([]byte, error) {
	b, _ := json.Marshal(n.Value) // a *string always encodes
	return b, nil
}

type createPayload struct {
	Kind              string                 `json:"kind"`
	Name              string                 `json:"name"`
	LegalName         *string                `json:"legal_name,omitempty"`
	CountryCode       string                 `json:"country_code"`
	Language          *string                `json:"language,omitempty"`
	Roles             []string               `json:"roles"`
	SupplierCompanies []SupplierCompanyInput `json:"supplier_companies"`
	Addresses         []AddressInput         `json:"addresses"`
	TaxIdentifiers    []sealedTax            `json:"tax_identifiers"`
	BankAccounts      []sealedBank           `json:"bank_accounts"`
	Extensions        map[string]any         `json:"extensions"`
}

type updatePayload struct {
	Name              *string                `json:"name,omitempty"`
	LegalName         nullable               `json:"legal_name,omitzero"`
	Language          nullable               `json:"language,omitzero"`
	Roles             []string               `json:"roles,omitempty"`
	SupplierCompanies []SupplierCompanyInput `json:"supplier_companies,omitempty"`
	Addresses         []AddressInput         `json:"addresses,omitempty"`
	TaxIdentifiers    []sealedTax            `json:"tax_identifiers,omitempty"`
	Extensions        map[string]any         `json:"extensions,omitempty"`
	// ExtensionsSchema is the schema version the extensions were checked
	// against when the change was requested; nil before T58c and while the
	// tenant declares no schema, which keeps the partner's.
	ExtensionsSchema *int `json:"extensions_schema,omitempty"`
}

type bankChangePayload struct {
	Action        string      `json:"action"` // add or end
	BankAccount   *sealedBank `json:"bank_account,omitempty"`
	BankAccountID *uuid.UUID  `json:"bank_account_id,omitempty"`
	ValidTo       *Date       `json:"valid_to,omitempty"`
}

// decode reads a stored payload into its struct.
func decode(payload map[string]any, into any) {
	b, _ := json.Marshal(payload) // stored payloads are JSON
	_ = json.Unmarshal(b, into)   // and were written from these structs
}

func invalid(code, field, message string) error {
	return problem.Unprocessable(code, message, problem.FieldError{Field: field, Code: "invalid", Message: message})
}

// checkCountries reports the first field whose country is not in the
// reference data.
func checkCountries(ctx context.Context, tx pgx.Tx, fields, codes []string) error {
	var known []string
	if err := tx.QueryRow(ctx, `SELECT coalesce(array_agg(code::text), '{}') FROM ref.country WHERE code = ANY($1)`, codes).Scan(&known); err != nil {
		return fmt.Errorf("partner: look up countries %v: %w", codes, err)
	}
	for i, c := range codes {
		if !slices.Contains(known, c) {
			return problem.Unprocessable("partner.unknown_country", "The country code is not known.",
				problem.FieldError{Field: fields[i], Code: "unknown", Message: "not an ISO 3166 country in the reference data"})
		}
	}
	return nil
}

// checkCompanies validates supplier data: a supplier role, legal entities
// the caller may act in, the ledger's supplier reconciliation accounts and
// configured payment terms.
func checkCompanies(ctx context.Context, tx pgx.Tx, ports Ports, supplier bool, in []SupplierCompanyInput) error {
	if len(in) > 0 && !supplier {
		return invalid("partner.supplier_role_required", "roles", "supplier data needs the supplier role")
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return err
	}
	if err := lockGiven(ctx, tx, in); err != nil {
		return err
	}
	seen := map[uuid.UUID]bool{}
	for i, s := range in {
		f := fmt.Sprintf("supplier_companies[%d]", i)
		if seen[s.LegalEntityID] {
			return invalid("partner.duplicate_company", f+".legal_entity_id", "one entry per legal entity")
		}
		seen[s.LegalEntityID] = true
		if _, err := tenancy.Accessible(ctx, ports.LegalEntities, access, s.LegalEntityID, f+".legal_entity_id"); err != nil {
			return err
		}
		recon, err := ports.Accounts.IsReconciliationAccount(ctx, s.LegalEntityID, s.ReconciliationAccountID, "supplier")
		if err != nil {
			return err
		}
		if !recon {
			return invalid("partner.invalid_reconciliation_account", f+".reconciliation_account_id",
				"an active supplier reconciliation account of the legal entity")
		}
		var term bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM partner.payment_term WHERE code = $1)`, s.PaymentTermCode).Scan(&term); err != nil {
			return fmt.Errorf("partner: find payment term %s: %w", s.PaymentTermCode, err)
		}
		if !term {
			return invalid("partner.unknown_payment_term", f+".payment_term_code", "payment terms from the configuration")
		}
		if err := checkCompanyCodes(ctx, ports, f, s); err != nil {
			return err
		}
	}
	return nil
}

// checkCompanyCodes checks the supplier's withholding tax codes, payment
// methods and planning group in the company.
func checkCompanyCodes(ctx context.Context, ports Ports, f string, s SupplierCompanyInput) error {
	if err := checkCodes(ctx, withholdingCodes, f, s.LegalEntityID, s.WithholdingTaxCodes, ports.Accounts.IsWithholdingTaxCode); err != nil {
		return err
	}
	if err := checkCodes(ctx, paymentMethodCodes, f, s.LegalEntityID, s.PaymentMethods, ports.PaymentMethods.IsPaymentMethod); err != nil {
		return err
	}
	if s.PlanningGroup == "" {
		return nil
	}
	ok, err := ports.PlanningGroups.IsPlanningGroup(ctx, s.PlanningGroup)
	if err != nil {
		return err
	}
	if !ok {
		return invalid("partner.unknown_planning_group", f+".planning_group", "a planning group of the configuration (cash.planning_groups)")
	}
	return nil
}

// lockGiven takes, for each kind of code the companies give, the kind's
// lock, so a configuration taking a code away waits for the change and
// then sees it pending (WithholdingCodes, PaymentMethodCodes,
// PlanningGroupCodes). The locks are taken in the order an activation
// takes them, withholding first and planning groups last, so the two never
// wait on each other in a cycle.
func lockGiven(ctx context.Context, tx pgx.Tx, in []SupplierCompanyInput) error {
	var withholding, methods, groups bool
	for _, s := range in {
		withholding = withholding || len(s.WithholdingTaxCodes) > 0
		methods = methods || len(s.PaymentMethods) > 0
		groups = groups || s.PlanningGroup != ""
	}
	for _, k := range []struct {
		given bool
		kind  supplierCodes
	}{{withholding, withholdingCodes}, {methods, paymentMethodCodes}, {groups, planningGroupCodes}} {
		if !k.given {
			continue
		}
		if err := lockCodes(ctx, tx, k.kind.lock); err != nil {
			return err
		}
	}
	return nil
}

// checkCodes requires a supplier's codes of the kind to be active codes
// of the legal entity's company, each once; lockGiven has taken the
// kind's lock.
func checkCodes(ctx context.Context, kind supplierCodes, f string, le uuid.UUID, codes []string,
	exists func(ctx context.Context, legalEntity uuid.UUID, code string) (bool, error)) error {
	slug := strings.ReplaceAll(kind.noun, " ", "_")
	seen := map[string]bool{}
	for j, code := range codes {
		field := fmt.Sprintf("%s.%s[%d]", f, kind.field, j)
		if seen[code] {
			return invalid("partner.duplicate_"+slug, field, "each "+kind.noun+" once")
		}
		seen[code] = true
		ok, err := exists(ctx, le, code)
		if err != nil {
			return err
		}
		if !ok {
			return invalid("partner.unknown_"+slug, field, "a "+kind.noun+" of the company's configuration")
		}
	}
	return nil
}

// checkAddresses validates purposes and validity; current requires that no
// address starts in the past (added addresses replace current ones).
func checkAddresses(day time.Time, in []AddressInput, current bool) error {
	for i, a := range in {
		f := fmt.Sprintf("addresses[%d]", i)
		switch {
		case !slices.Contains(addressPurposes, a.Purpose):
			return invalid("partner.unknown_address_purpose", f+".purpose", "legal, remit_to, order_from or ship_from")
		case a.ValidTo != nil && !a.ValidTo.After(a.ValidFrom.Time):
			return invalid("partner.invalid_validity", f+".valid_to", "after valid_from")
		case current && a.ValidFrom.Before(day):
			return invalid("partner.backdated_address", f+".valid_from", "today or later")
		}
	}
	return nil
}

func sealTaxes(ctx context.Context, keys *fieldcrypt.Keyring, tenant uuid.UUID, in []TaxInput) ([]sealedTax, error) {
	out := []sealedTax{}
	seen := map[string]bool{}
	for i, t := range in {
		if seen[t.Scheme] {
			return nil, invalid("partner.duplicate_tax_scheme", fmt.Sprintf("tax_identifiers[%d].scheme", i), "one identifier per scheme")
		}
		seen[t.Scheme] = true
		cipher, err := keys.Seal(ctx, tenant, t.Value)
		if err != nil {
			return nil, err
		}
		print, err := keys.Fingerprint(ctx, tenant, t.Value)
		if err != nil {
			return nil, err
		}
		out = append(out, sealedTax{Scheme: t.Scheme, ValueCipher: cipher, ValueFingerprint: print, ValueLast4: fieldcrypt.Last4(t.Value)})
	}
	return out, nil
}

// sealBanks checks and seals new bank accounts, each starting no earlier
// than back allows before partner's day.
func sealBanks(ctx context.Context, keys *fieldcrypt.Keyring, tenant uuid.UUID, day time.Time, back backdating, in []BankInput,
	field func(int) string) ([]sealedBank, error) {
	out := []sealedBank{}
	seen := map[string]bool{}
	for i, b := range in {
		f := field(i)
		if b.Purpose == "" {
			b.Purpose = "payments"
		}
		switch {
		case !slices.Contains(bankPurposes, b.Purpose):
			return nil, invalid("partner.unknown_bank_purpose", f+"purpose", "payments or collections")
		case seen[b.Purpose]:
			return nil, invalid("partner.duplicate_bank_purpose", f+"purpose", "one new account per purpose")
		case b.ValidFrom.Before(back.earliest(day)):
			return nil, invalid("partner.backdated_bank_account", f+"valid_from", back.allowed())
		}
		seen[b.Purpose] = true
		acct, err := checkAccount(b, f)
		if err != nil {
			return nil, err
		}
		cipher, err := keys.Seal(ctx, tenant, acct.number)
		if err != nil {
			return nil, err
		}
		// The fingerprint is of the sealed number alone, a Canadian
		// account's without its bank: key rotation recomputes it from the
		// sealed value, and a warning about the same number at two banks
		// costs a look, where a missed duplicate costs a fraud control.
		print, err := keys.Fingerprint(ctx, tenant, acct.number)
		if err != nil {
			return nil, err
		}
		out = append(out, sealedBank{Purpose: b.Purpose, AccountCipher: cipher, AccountFingerprint: print,
			AccountLast4: fieldcrypt.Last4(acct.number), AccountScheme: b.AccountScheme, BIC: b.BIC, BankCountryCode: b.BankCountryCode,
			HolderName: b.HolderName, ValidFrom: Date{b.ValidFrom}, InstitutionNumber: acct.institution, TransitNumber: acct.transit})
	}
	return out, nil
}

// checkedAccount is a bank input's account, normalized: the institution
// and transit numbers are set only for scheme ca.
type checkedAccount struct {
	number               string
	institution, transit *string
}

// checkAccount checks the account number by its scheme (ADR-0038): an
// IBAN by ISO 13616, a Canadian account by the digits and lengths of its
// three numbers (T45d), a domestic one by its characters.
func checkAccount(b BankInput, f string) (checkedAccount, error) {
	if !slices.Contains(accountSchemes, b.AccountScheme) {
		return checkedAccount{}, invalid("partner.unknown_account_scheme", f+"account_scheme", "iban, domestic or ca")
	}
	if b.AccountScheme == "ca" {
		return checkCanadian(b, f)
	}
	number := fieldcrypt.Normalize(b.AccountNumber)
	switch {
	case b.InstitutionNumber != "" || b.TransitNumber != "":
		return checkedAccount{}, invalid("partner.routing_not_allowed", f+"institution_number", "no institution or transit number but for account_scheme ca")
	case !accountNumber.MatchString(number):
		return checkedAccount{}, invalid("partner.invalid_account_number", f+"account_number", "4-34 letters and digits")
	case b.AccountScheme == "iban" && !bankfile.ValidIBAN(number):
		return checkedAccount{}, invalid("partner.invalid_iban", f+"account_number", "an IBAN that passes the ISO 13616 check")
	}
	return checkedAccount{number: number}, nil
}

// checkCanadian checks a Canadian account's three numbers.
func checkCanadian(b BankInput, f string) (checkedAccount, error) {
	a, err := cabank.Parse(b.InstitutionNumber, b.TransitNumber, b.AccountNumber)
	switch {
	case b.BankCountryCode != "CA":
		return checkedAccount{}, invalid("partner.bank_country_mismatch", f+"bank_country_code", "CA for account_scheme ca")
	case errors.Is(err, cabank.ErrInstitution):
		return checkedAccount{}, invalid("partner.invalid_institution_number", f+"institution_number", "3 digits")
	case errors.Is(err, cabank.ErrTransit):
		return checkedAccount{}, invalid("partner.invalid_transit_number", f+"transit_number", "5 digits")
	case err != nil:
		return checkedAccount{}, invalid("partner.invalid_account_number", f+"account_number", "7-12 digits")
	}
	return checkedAccount{number: a.Number, institution: &a.Institution, transit: &a.Transit}, nil
}

// warnings flags tax identifiers and bank accounts another partner already has.
func warnings(ctx context.Context, tx pgx.Tx, partnerID uuid.UUID, taxes []sealedTax, banks []sealedBank) ([]Warning, error) {
	var taxPrints, bankPrints []string
	for _, t := range taxes {
		taxPrints = append(taxPrints, t.ValueFingerprint)
	}
	for _, b := range banks {
		bankPrints = append(bankPrints, b.AccountFingerprint)
	}
	var dupTax, dupBank []string
	if err := tx.QueryRow(ctx, `SELECT
			coalesce((SELECT array_agg(DISTINCT value_fingerprint) FROM partner.tax_identifier WHERE value_fingerprint = ANY($1) AND partner_id <> $3), '{}'),
			coalesce((SELECT array_agg(DISTINCT account_fingerprint) FROM partner.bank_account WHERE account_fingerprint = ANY($2) AND partner_id <> $3), '{}')`,
		taxPrints, bankPrints, partnerID).Scan(&dupTax, &dupBank); err != nil {
		return nil, fmt.Errorf("partner: find duplicates of business partner %s: %w", partnerID, err)
	}
	out := []Warning{}
	for _, t := range taxes {
		if slices.Contains(dupTax, t.ValueFingerprint) {
			out = append(out, Warning{Code: "partner.duplicate_tax_identifier",
				Message: fmt.Sprintf("Another business partner has the same %s tax identifier (…%s).", t.Scheme, t.ValueLast4)})
		}
	}
	for _, b := range banks {
		if slices.Contains(dupBank, b.AccountFingerprint) {
			out = append(out, Warning{Code: "partner.duplicate_bank_account",
				Message: fmt.Sprintf("Another business partner has a bank account with the same number (…%s).", b.AccountLast4)})
		}
	}
	return out, nil
}

// insertChange records a change; applied changes (blocks) are applied now.
func insertChange(ctx context.Context, tx pgx.Tx, p reqctx.Principal, kind string, partnerID uuid.UUID, payload any, reason string,
	warns []Warning, applied bool) (Change, error) {
	number, err := nextNumber(ctx, tx, p.TenantID, "partner_change", "PC")
	if err != nil {
		return Change{}, err
	}
	status := ChangePending
	if applied {
		status = ChangeApplied
	}
	if warns == nil {
		warns = []Warning{}
	}
	c, err := scanChange(tx.QueryRow(ctx, `INSERT INTO partner.partner_change (tenant_id, id, number, kind, partner_id, payload, reason, warnings, status,
			requested_by, applied_at, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, CASE WHEN $11 THEN now() END, $10, $10) RETURNING `+changeColumns,
		p.TenantID, uuid.Must(uuid.NewV7()), number, kind, partnerID, toJSON(payload), reason, warns, status, p.ID, applied))
	if err != nil {
		return c, fmt.Errorf("partner: insert %s change %s of business partner %s: %w", kind, number, partnerID, err)
	}
	return c, nil
}

// The insert helpers write partner data from payloads, for creation and
// for applied changes.

func addRoles(ctx context.Context, tx pgx.Tx, p reqctx.Principal, partnerID uuid.UUID, rs []string) error {
	_, err := tx.Exec(ctx, `INSERT INTO partner.partner_role (tenant_id, partner_id, role, created_by)
		SELECT $1, $2, r, $3 FROM unnest($4::text[]) AS r ON CONFLICT DO NOTHING`, p.TenantID, partnerID, p.ID, rs)
	if err != nil {
		return fmt.Errorf("partner: add roles of business partner %s: %w", partnerID, err)
	}
	return nil
}

func putCompanies(ctx context.Context, tx pgx.Tx, p reqctx.Principal, partnerID uuid.UUID, in []SupplierCompanyInput) error {
	var les, accounts []uuid.UUID
	var terms, methods, groups []string
	for _, s := range in {
		les, accounts, terms = append(les, s.LegalEntityID), append(accounts, s.ReconciliationAccountID), append(terms, s.PaymentTermCode)
		// A method is one letter or digit, so a comma separates them
		// unambiguously; string_to_array makes '' the empty list.
		methods, groups = append(methods, strings.Join(s.PaymentMethods, ",")), append(groups, s.PlanningGroup)
	}
	_, err := tx.Exec(ctx, `INSERT INTO partner.supplier_company AS s (tenant_id, partner_id, legal_entity_id, reconciliation_account_id,
			payment_term_code, payment_methods, planning_group, created_by, updated_by)
		SELECT $1, $2, le, a, t, string_to_array(m, ','), nullif(g, ''), $3, $3
		FROM unnest($4::uuid[], $5::uuid[], $6::text[], $7::text[], $8::text[]) AS x(le, a, t, m, g)
		ON CONFLICT (tenant_id, partner_id, legal_entity_id) DO UPDATE SET reconciliation_account_id = EXCLUDED.reconciliation_account_id,
			payment_term_code = EXCLUDED.payment_term_code, payment_methods = EXCLUDED.payment_methods, planning_group = EXCLUDED.planning_group,
			version = s.version + 1, updated_by = $3`,
		p.TenantID, partnerID, p.ID, les, accounts, terms, methods, groups)
	if err != nil {
		return fmt.Errorf("partner: put supplier companies of business partner %s: %w", partnerID, err)
	}
	return putWithholding(ctx, tx, p, partnerID, in)
}

// putWithholding makes the supplier's withholding tax codes in each legal
// entity of in the given ones: codes no longer given are deactivated,
// never deleted, and given ones inserted or reactivated (T46e).
func putWithholding(ctx context.Context, tx pgx.Tx, p reqctx.Principal, partnerID uuid.UUID, in []SupplierCompanyInput) error {
	var les []uuid.UUID
	var pairsLE []uuid.UUID
	var codes []string
	for _, s := range in {
		les = append(les, s.LegalEntityID)
		for _, c := range s.WithholdingTaxCodes {
			pairsLE, codes = append(pairsLE, s.LegalEntityID), append(codes, c)
		}
	}
	// Giving codes waits for a configuration taking codes away, which then
	// sees them (WithholdingCodes); taking them off needs no lock.
	if len(codes) > 0 {
		if err := lockCodes(ctx, tx, withholdingLock); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE partner.supplier_withholding w SET is_active = false, version = version + 1, updated_by = $4
		WHERE w.partner_id = $1 AND w.legal_entity_id = ANY($2) AND w.is_active
			AND NOT EXISTS (SELECT FROM unnest($3::uuid[], $5::text[]) AS x(le, code) WHERE x.le = w.legal_entity_id AND x.code = w.code)`,
		partnerID, les, pairsLE, p.ID, codes); err != nil {
		return fmt.Errorf("partner: take withholding tax codes off business partner %s: %w", partnerID, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO partner.supplier_withholding AS w (tenant_id, partner_id, legal_entity_id, code, created_by, updated_by)
		SELECT $1, $2, le, code, $3, $3 FROM unnest($4::uuid[], $5::text[]) AS x(le, code)
		ON CONFLICT (tenant_id, partner_id, legal_entity_id, code) DO UPDATE SET is_active = true, version = w.version + 1, updated_by = $3
		WHERE NOT w.is_active`, p.TenantID, partnerID, p.ID, pairsLE, codes); err != nil {
		return fmt.Errorf("partner: put withholding tax codes of business partner %s: %w", partnerID, err)
	}
	return nil
}

// addAddresses inserts addresses; with replace, each ends the current
// address of its purpose on its first day.
func addAddresses(ctx context.Context, tx pgx.Tx, p reqctx.Principal, partnerID uuid.UUID, in []AddressInput, replace bool) error {
	for _, a := range in {
		if replace {
			if _, err := tx.Exec(ctx, `UPDATE partner.address SET valid_to = $3, ended_by = $4
				WHERE partner_id = $1 AND purpose = $2 AND valid_from < $3 AND (valid_to IS NULL OR valid_to > $3)`,
				partnerID, a.Purpose, a.ValidFrom.Time, p.ID); err != nil {
				return fmt.Errorf("partner: end the %s address of business partner %s: %w", a.Purpose, partnerID, err)
			}
		}
		var to *time.Time
		if a.ValidTo != nil {
			to = &a.ValidTo.Time
		}
		if _, err := tx.Exec(ctx, `INSERT INTO partner.address (tenant_id, id, partner_id, purpose, line1, line2, city, postal_code, region,
				country_code, valid_from, valid_to, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			p.TenantID, uuid.Must(uuid.NewV7()), partnerID, a.Purpose, a.Line1, a.Line2, a.City, a.PostalCode, a.Region, a.CountryCode,
			a.ValidFrom.Time, to, p.ID); err != nil {
			return fmt.Errorf("partner: insert the %s address of business partner %s: %w", a.Purpose, partnerID, err)
		}
	}
	return nil
}

func putTaxes(ctx context.Context, tx pgx.Tx, p reqctx.Principal, partnerID uuid.UUID, in []sealedTax) error {
	var schemes, prints, last4 []string
	var ciphers [][]byte
	for _, t := range in {
		schemes, prints, last4, ciphers = append(schemes, t.Scheme), append(prints, t.ValueFingerprint), append(last4, t.ValueLast4), append(ciphers, t.ValueCipher)
	}
	_, err := tx.Exec(ctx, `INSERT INTO partner.tax_identifier AS t (tenant_id, partner_id, scheme, value_cipher, value_fingerprint, value_last4,
			created_by, updated_by)
		SELECT $1, $2, s, c, f, l, $3, $3 FROM unnest($4::text[], $5::bytea[], $6::text[], $7::text[]) AS x(s, c, f, l)
		ON CONFLICT (tenant_id, partner_id, scheme) DO UPDATE SET value_cipher = EXCLUDED.value_cipher,
			value_fingerprint = EXCLUDED.value_fingerprint, value_last4 = EXCLUDED.value_last4, version = t.version + 1, updated_by = $3`,
		p.TenantID, partnerID, p.ID, schemes, ciphers, prints, last4)
	if err != nil {
		return fmt.Errorf("partner: put tax identifiers of business partner %s: %w", partnerID, err)
	}
	return nil
}

// NewPartner is the input of CreatePartner.
type NewPartner struct {
	Kind, Name        string
	LegalName         *string
	CountryCode       string
	Language          *string
	Roles             []string
	SupplierCompanies []SupplierCompanyInput
	Addresses         []AddressInput
	TaxIdentifiers    []TaxInput
	BankAccounts      []BankInput
	Extensions        map[string]any
	Reason            string
}

// CreatePartner inserts a pending partner with its roles, company data,
// addresses and tax identifiers, and the pending create change that holds
// its bank accounts until approval.
func CreatePartner(ctx context.Context, ports Ports, in NewPartner) (Partner, Change, error) {
	return createPartner(ctx, ports, in, noBackdating)
}

// createPartner is CreatePartner with the bank accounts' backdating, which
// only Migration.Create relaxes.
func createPartner(ctx context.Context, ports Ports, in NewPartner, back backdating) (Partner, Change, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Partner{}, Change{}, err
	}
	if err := checkNewPartner(&in); err != nil {
		return Partner{}, Change{}, err
	}
	schema, err := ports.extensions().Check(ctx, in.Extensions)
	if err != nil {
		return Partner{}, Change{}, err
	}
	day, err := database.Today(ctx, tx)
	if err != nil {
		return Partner{}, Change{}, err
	}
	if err := checkNewPartnerData(ctx, tx, ports, day, in); err != nil {
		return Partner{}, Change{}, err
	}
	taxes, err := sealTaxes(ctx, ports.Keys, p.TenantID, in.TaxIdentifiers)
	if err != nil {
		return Partner{}, Change{}, err
	}
	banks, err := sealBanks(ctx, ports.Keys, p.TenantID, day, back, in.BankAccounts, func(i int) string { return fmt.Sprintf("bank_accounts[%d].", i) })
	if err != nil {
		return Partner{}, Change{}, err
	}
	number, err := nextNumber(ctx, tx, p.TenantID, "business_partner", "BP")
	if err != nil {
		return Partner{}, Change{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if err := insertPartner(ctx, tx, p, id, number, in, schema, taxes); err != nil {
		return Partner{}, Change{}, err
	}
	warns, err := warnings(ctx, tx, id, taxes, banks)
	if err != nil {
		return Partner{}, Change{}, err
	}
	c, err := insertChange(ctx, tx, p, KindCreate, id, createPayload{Kind: in.Kind, Name: in.Name, LegalName: in.LegalName,
		CountryCode: in.CountryCode, Language: in.Language, Roles: in.Roles, SupplierCompanies: in.SupplierCompanies, Addresses: in.Addresses,
		TaxIdentifiers: taxes, BankAccounts: banks, Extensions: in.Extensions}, in.Reason, warns, false)
	if err != nil {
		return Partner{}, Change{}, err
	}
	bp, err := getPartner(ctx, tx, id, false)
	if err != nil {
		return Partner{}, Change{}, err
	}
	return bp, c, recordCreated(ctx, ports.extensions(), bp, c, len(warns))
}

// checkNewPartnerData checks a new partner's countries, company data and
// addresses, then the tenant's rules on its header (ADR-0046).
func checkNewPartnerData(ctx context.Context, tx pgx.Tx, ports Ports, day time.Time, in NewPartner) error {
	fields, codes := newPartnerCountries(in)
	if err := checkCountries(ctx, tx, fields, codes); err != nil {
		return err
	}
	if err := checkCompanies(ctx, tx, ports, slices.Contains(in.Roles, "supplier"), in.SupplierCompanies); err != nil {
		return err
	}
	if err := checkAddresses(day, in.Addresses, false); err != nil {
		return err
	}
	return ports.checkRules(ctx, "partner.business_partner.create", "", uuid.Nil, Partner{Kind: in.Kind, Name: in.Name, LegalName: in.LegalName,
		CountryCode: in.CountryCode, Language: in.Language, Roles: in.Roles, Extensions: in.Extensions})
}

// recordCreated audits the new partner and publishes
// BusinessPartnerCreated, with its custom fields masked (ADR-0046).
func recordCreated(ctx context.Context, w extensions.Writes, bp Partner, c Change, warnings int) error {
	payload := map[string]any{"partner_id": bp.ID, "number": bp.Number, "kind": bp.Kind, "name": bp.Name, "country_code": bp.CountryCode,
		"roles": bp.Roles, "status": Pending, "partner_change_id": c.ID}
	if err := w.Event(ctx, payload, bp.Extensions, bp.ExtensionsSchema); err != nil {
		return err
	}
	return audit.Record(ctx, "partner.business_partner.create", "partner.business_partner", bp.ID,
		map[string]any{"number": bp.Number, "partner_change_id": c.ID, "warnings": warnings},
		event("business_partner_created", bp.ID, "partner.business_partner", payload))
}

// checkNewPartner requires a known partner kind, a name and known roles;
// the extensions default to none.
func checkNewPartner(in *NewPartner) error {
	if !slices.Contains(kinds, in.Kind) {
		return invalid("partner.unknown_kind", "kind", "organization or person")
	}
	if err := problem.RequireText("name", in.Name); err != nil {
		return err
	}
	for i, r := range in.Roles {
		if !slices.Contains(roles, r) {
			return invalid("partner.unknown_role", fmt.Sprintf("roles[%d]", i), "supplier or customer")
		}
	}
	if in.Extensions == nil {
		in.Extensions = map[string]any{}
	}
	return nil
}

// newPartnerCountries lists the country codes of a new partner with the
// fields they come from: its own, its addresses' and its banks'.
func newPartnerCountries(in NewPartner) (fields, codes []string) {
	fields, codes = []string{"country_code"}, []string{in.CountryCode}
	for i, a := range in.Addresses {
		fields, codes = append(fields, fmt.Sprintf("addresses[%d].country_code", i)), append(codes, a.CountryCode)
	}
	for i, b := range in.BankAccounts {
		fields, codes = append(fields, fmt.Sprintf("bank_accounts[%d].bank_country_code", i)), append(codes, b.BankCountryCode)
	}
	return fields, codes
}

// insertPartner writes a pending partner with its roles, company data,
// addresses and sealed tax identifiers, and the custom field schema version
// its extensions were checked against.
func insertPartner(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, number string, in NewPartner, schema *int, taxes []sealedTax) error {
	if _, err := tx.Exec(ctx, `INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, legal_name, country_code, language,
			status, extensions, extensions_schema, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending', $9, $10, $11, $11)`,
		p.TenantID, id, number, in.Kind, in.Name, in.LegalName, in.CountryCode, in.Language, in.Extensions, schema, p.ID); err != nil {
		return fmt.Errorf("partner: insert business partner %s: %w", number, err)
	}
	if err := addRoles(ctx, tx, p, id, in.Roles); err != nil {
		return err
	}
	if err := putCompanies(ctx, tx, p, id, in.SupplierCompanies); err != nil {
		return err
	}
	if err := addAddresses(ctx, tx, p, id, in.Addresses, false); err != nil {
		return err
	}
	return putTaxes(ctx, tx, p, id, taxes)
}

// Block blocks an active partner at once and records the applied change.
func Block(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, reason string) (Change, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Change{}, err
	}
	bp, err := getPartner(ctx, tx, id, true)
	if err != nil {
		return Change{}, err
	}
	if err := ifMatch(bp.Version); err != nil {
		return Change{}, err
	}
	if bp.Status != Active {
		return Change{}, problem.Conflict("partner.invalid_status", "The business partner is "+bp.Status+"; block requires status active.")
	}
	if _, err := tx.Exec(ctx, `UPDATE partner.business_partner SET status = 'blocked', version = version + 1, updated_by = $2 WHERE id = $1`,
		id, p.ID); err != nil {
		return Change{}, fmt.Errorf("partner: block business partner %s: %w", id, err)
	}
	c, err := insertChange(ctx, tx, p, KindBlock, id, map[string]any{"scope": "all"}, reason, nil, true)
	if err != nil {
		return Change{}, err
	}
	return c, audit.Record(ctx, "partner.business_partner.block", "partner.business_partner", id, map[string]any{"partner_change_id": c.ID, "reason": reason},
		event("partner_blocked", id, "partner.business_partner", map[string]any{
			"partner_id": id, "scope": "all", "legal_entity_id": nil, "partner_change_id": c.ID, "reason": reason}))
}

// Unblock opens the change that unblocks a blocked partner on approval.
func Unblock(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, reason string) (Change, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Change{}, err
	}
	bp, err := getPartner(ctx, tx, id, true)
	if err != nil {
		return Change{}, err
	}
	if err := ifMatch(bp.Version); err != nil {
		return Change{}, err
	}
	if bp.Status != Blocked {
		return Change{}, problem.Conflict("partner.invalid_status", "The business partner is "+bp.Status+"; unblock requires status blocked.")
	}
	c, err := insertChange(ctx, tx, p, KindUnblock, id, map[string]any{"scope": "all"}, reason, nil, false)
	if err != nil {
		return Change{}, err
	}
	return c, audit.Record(ctx, "partner.change.request", "partner.partner_change", c.ID, map[string]any{"kind": c.Kind, "partner_id": id})
}

// Changes is the input of RequestUpdate (merge-patch semantics).
type Changes struct {
	Name              *string
	LegalName         **string // nil: unchanged; pointer to nil: clear
	Language          **string
	Roles             []string // complete set; nil: unchanged
	SupplierCompanies []SupplierCompanyInput
	Addresses         []AddressInput
	TaxIdentifiers    []TaxInput
	Extensions        map[string]any
}

func toNullable(v **string) nullable {
	if v == nil {
		return nullable{}
	}
	return nullable{Set: true, Value: *v}
}

// RequestUpdate opens an update change of a partner's master data,
// validated now and applied on approval.
func RequestUpdate(ctx context.Context, ports Ports, partnerID uuid.UUID, ch Changes, reason string) (Change, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Change{}, err
	}
	bp, err := getPartner(ctx, tx, partnerID, false)
	if err != nil {
		return Change{}, problem.Reference(err, "partner_id", "business partner", "no business partner with this id")
	}
	if err := checkChanges(ch); err != nil {
		return Change{}, err
	}
	all, err := updatedRoles(bp.Roles, ch.Roles)
	if err != nil {
		return Change{}, err
	}
	// The partner's extensions as the change leaves them are checked against
	// the active schema now, on every update (ADR-0046: a narrowed field
	// applies from the next write), and the version is applied with them.
	schema, err := ports.extensions().Check(ctx, mergepatch.Apply(bp.Extensions, ch.Extensions))
	if err != nil {
		return Change{}, err
	}
	day, err := database.Today(ctx, tx)
	if err != nil {
		return Change{}, err
	}
	var fields, codes []string
	for i, a := range ch.Addresses {
		fields, codes = append(fields, fmt.Sprintf("payload.addresses[%d].country_code", i)), append(codes, a.CountryCode)
	}
	if err := checkCountries(ctx, tx, fields, codes); err != nil {
		return Change{}, err
	}
	if err := checkCompanies(ctx, tx, ports, slices.Contains(all, "supplier"), ch.SupplierCompanies); err != nil {
		return Change{}, err
	}
	if err := checkAddresses(day, ch.Addresses, true); err != nil {
		return Change{}, err
	}
	if err := ports.checkRules(ctx, "partner.change.request", "partner.business_partner", partnerID, requested(bp, ch, all)); err != nil {
		return Change{}, err
	}
	taxes, err := sealTaxes(ctx, ports.Keys, p.TenantID, ch.TaxIdentifiers)
	if err != nil {
		return Change{}, err
	}
	warns, err := warnings(ctx, tx, partnerID, taxes, nil)
	if err != nil {
		return Change{}, err
	}
	c, err := insertChange(ctx, tx, p, KindUpdate, partnerID, updatePayload{Name: ch.Name, LegalName: toNullable(ch.LegalName),
		Language: toNullable(ch.Language), Roles: ch.Roles, SupplierCompanies: ch.SupplierCompanies, Addresses: ch.Addresses,
		TaxIdentifiers: taxes, Extensions: ch.Extensions, ExtensionsSchema: schema}, reason, warns, false)
	if err != nil {
		return Change{}, err
	}
	return c, audit.Record(ctx, "partner.change.request", "partner.partner_change", c.ID, map[string]any{"kind": c.Kind, "partner_id": partnerID})
}

// checkChanges refuses an update change that requests nothing, or a
// name of white space alone, which the partner's check refuses at
// approval otherwise.
func checkChanges(ch Changes) error {
	if ch.Name == nil && ch.LegalName == nil && ch.Language == nil && ch.Roles == nil && len(ch.SupplierCompanies) == 0 &&
		len(ch.Addresses) == 0 && len(ch.TaxIdentifiers) == 0 && len(ch.Extensions) == 0 {
		return problem.Unprocessable("partner.no_changes", "The change requests no changes.")
	}
	if ch.Name != nil {
		return problem.RequireText("payload.name", *ch.Name)
	}
	return nil
}

// requested is the partner as an update change leaves its header: what
// the rules judge (ADR-0046).
func requested(bp Partner, ch Changes, roles []string) Partner {
	if ch.Name != nil {
		bp.Name = *ch.Name
	}
	if ch.LegalName != nil {
		bp.LegalName = *ch.LegalName
	}
	if ch.Language != nil {
		bp.Language = *ch.Language
	}
	bp.Roles, bp.Extensions = roles, mergepatch.Apply(bp.Extensions, ch.Extensions)
	return bp
}

// updatedRoles is the partner's set of roles after a change, which may
// add known roles but not remove any; nil keeps the current ones.
func updatedRoles(current, changed []string) ([]string, error) {
	if changed == nil {
		return current, nil
	}
	for i, r := range changed {
		if !slices.Contains(roles, r) {
			return nil, invalid("partner.unknown_role", fmt.Sprintf("payload.roles[%d]", i), "supplier or customer")
		}
	}
	for _, r := range current {
		if !slices.Contains(changed, r) {
			return nil, invalid("partner.role_removal", "payload.roles", "roles can be added but not removed; "+r+" is missing")
		}
	}
	return changed, nil
}

// RequestBankAccount opens a bank change that adds an account on approval.
func RequestBankAccount(ctx context.Context, ports Ports, partnerID uuid.UUID, in BankInput, reason string) (Change, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Change{}, err
	}
	if _, err := getPartner(ctx, tx, partnerID, false); err != nil {
		return Change{}, err
	}
	day, err := database.Today(ctx, tx)
	if err != nil {
		return Change{}, err
	}
	if err := checkCountries(ctx, tx, []string{"bank_country_code"}, []string{in.BankCountryCode}); err != nil {
		return Change{}, err
	}
	banks, err := sealBanks(ctx, ports.Keys, p.TenantID, day, noBackdating, []BankInput{in}, func(int) string { return "" })
	if err != nil {
		return Change{}, err
	}
	warns, err := warnings(ctx, tx, partnerID, nil, banks)
	if err != nil {
		return Change{}, err
	}
	c, err := insertChange(ctx, tx, p, KindBankChange, partnerID, bankChangePayload{Action: "add", BankAccount: &banks[0]}, reason, warns, false)
	if err != nil {
		return Change{}, err
	}
	return c, audit.Record(ctx, "partner.change.request", "partner.partner_change", c.ID, map[string]any{"kind": c.Kind, "partner_id": partnerID, "action": "add"})
}

// RequestBankAccountEnd opens a bank change that ends an account on
// approval. The end is not in the past and only shortens validity.
func RequestBankAccountEnd(ctx context.Context, partnerID, bankID uuid.UUID, ifMatch func(BankAccount) error, validTo time.Time, reason string) (Change, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Change{}, err
	}
	b, err := getBank(ctx, tx, partnerID, bankID)
	if err != nil {
		return Change{}, err
	}
	if err := ifMatch(b); err != nil {
		return Change{}, err
	}
	day, err := database.Today(ctx, tx)
	if err != nil {
		return Change{}, err
	}
	switch {
	case validTo.Before(day):
		return Change{}, invalid("partner.invalid_end", "valid_to", "today or later")
	case !validTo.After(b.ValidFrom):
		return Change{}, invalid("partner.invalid_end", "valid_to", "after the account's valid_from")
	case b.ValidTo != nil && !validTo.Before(*b.ValidTo):
		return Change{}, invalid("partner.invalid_end", "valid_to", "before the account's current valid_to")
	}
	c, err := insertChange(ctx, tx, p, KindBankChange, partnerID, bankChangePayload{Action: "end", BankAccountID: &bankID, ValidTo: &Date{validTo}},
		reason, nil, false)
	if err != nil {
		return Change{}, err
	}
	return c, audit.Record(ctx, "partner.change.request", "partner.partner_change", c.ID,
		map[string]any{"kind": c.Kind, "partner_id": partnerID, "action": "end", "bank_account_id": bankID})
}

// pendingChange locks a change that must still be pending.
func pendingChange(ctx context.Context, tx pgx.Tx, id uuid.UUID, action string) (Change, error) {
	c, err := getChange(ctx, tx, id, true)
	if err != nil {
		return c, err
	}
	if c.Status != ChangePending {
		return c, problem.Conflict("partner.invalid_status", "The partner change is "+c.Status+"; "+action+" requires status pending.")
	}
	return c, nil
}

// ApplyChange applies an approved pending change. The workflow context
// calls it when the change's approval request is approved (T4); schemas
// mask the custom fields of the events it publishes (ADR-0046).
func ApplyChange(ctx context.Context, schemas extensions.Schemas, id uuid.UUID) (Change, error) {
	return applyChange(ctx, schemas, id, noBackdating)
}

// applyChange is ApplyChange with the earliest first day of a created
// partner's bank accounts, which only Migration.Create relaxes, for the
// create change it made; updates and bank changes keep partner's day.
func applyChange(ctx context.Context, schemas extensions.Schemas, id uuid.UUID, back backdating) (Change, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Change{}, err
	}
	c, err := pendingChange(ctx, tx, id, "apply")
	if err != nil {
		return Change{}, err
	}
	bp, err := getPartner(ctx, tx, c.PartnerID, true)
	if err != nil {
		return Change{}, err
	}
	day, err := database.Today(ctx, tx)
	if err != nil {
		return Change{}, err
	}
	var events []outbox.Event
	switch c.Kind {
	case KindCreate:
		events, err = applyCreate(ctx, tx, p, c, bp, back.earliest(day))
	case KindUpdate:
		events, err = applyUpdate(ctx, tx, p, c, bp, day, partnerWrites(schemas))
	case KindBankChange:
		events, err = applyBankChange(ctx, tx, p, c, day)
	default: // unblock; blocks are applied when requested
		events, err = applyUnblock(ctx, tx, p, c, bp)
	}
	if err != nil {
		return Change{}, err
	}
	applied, err := scanChange(tx.QueryRow(ctx, `UPDATE partner.partner_change SET status = 'applied', applied_at = now(), version = version + 1,
		updated_by = $2 WHERE id = $1 RETURNING `+changeColumns, id, p.ID))
	if err != nil {
		return Change{}, fmt.Errorf("partner: mark partner change %s applied: %w", id, err)
	}
	return applied, audit.Record(ctx, "partner.change.apply", "partner.partner_change", id, map[string]any{"kind": c.Kind, "partner_id": c.PartnerID}, events...)
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// insertBank adds an approved account, ending the current account of its
// purpose on the new account's first day: the requested day, or day (the
// earliest allowed) when that has passed while the change waited.
func insertBank(ctx context.Context, tx pgx.Tx, p reqctx.Principal, c Change, b sealedBank, day time.Time) (outbox.Event, error) {
	from := later(b.ValidFrom.Time, day)
	var ended *uuid.UUID
	var endedTo *time.Time
	err := tx.QueryRow(ctx, `UPDATE partner.bank_account SET valid_to = $3, ended_by = $4
		WHERE partner_id = $1 AND purpose = $2 AND valid_from < $3 AND (valid_to IS NULL OR valid_to > $3) RETURNING id, valid_to`,
		c.PartnerID, b.Purpose, from, p.ID).Scan(&ended, &endedTo)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return outbox.Event{}, fmt.Errorf("partner: end the current %s bank account of business partner %s: %w", b.Purpose, c.PartnerID, err)
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO partner.bank_account (tenant_id, id, partner_id, purpose, account_cipher, account_fingerprint,
			account_last4, account_scheme, bic, bank_country_code, holder_name, valid_from, partner_change_id, created_by, institution_number, transit_number)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		p.TenantID, id, c.PartnerID, b.Purpose, b.AccountCipher, b.AccountFingerprint, b.AccountLast4, b.AccountScheme, b.BIC,
		b.BankCountryCode, b.HolderName, from, c.ID, p.ID, b.InstitutionNumber, b.TransitNumber); err != nil {
		return outbox.Event{}, fmt.Errorf("partner: insert the %s bank account of business partner %s: %w", b.Purpose, c.PartnerID, err)
	}
	return event("bank_account_changed", c.PartnerID, "partner.business_partner", map[string]any{
		"partner_id": c.PartnerID, "bank_account_id": id, "ended_bank_account_id": ended, "ended_valid_to": dateOrNil(endedTo),
		"purpose": b.Purpose, "account_scheme": b.AccountScheme, "account_last4": b.AccountLast4, "bic": b.BIC,
		"institution_number": b.InstitutionNumber, "transit_number": b.TransitNumber,
		"bank_country_code": b.BankCountryCode, "valid_from": from.Format(time.DateOnly), "valid_to": nil, "partner_change_id": c.ID}), nil
}

func dateOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format(time.DateOnly)
}

// applyCreate activates the partner and adds its bank accounts, none
// starting before earliest.
func applyCreate(ctx context.Context, tx pgx.Tx, p reqctx.Principal, c Change, bp Partner, earliest time.Time) ([]outbox.Event, error) {
	var pl createPayload
	decode(c.Payload, &pl)
	if _, err := tx.Exec(ctx, `UPDATE partner.business_partner SET status = 'active', version = version + 1, updated_by = $2 WHERE id = $1`,
		bp.ID, p.ID); err != nil {
		return nil, fmt.Errorf("partner: activate business partner %s: %w", bp.ID, err)
	}
	var events []outbox.Event
	if slices.Contains(bp.Roles, "supplier") {
		les := []uuid.UUID{}
		for _, s := range bp.SupplierCompanies {
			les = append(les, s.LegalEntityID)
		}
		events = append(events, event("supplier_approved", bp.ID, "partner.business_partner", map[string]any{
			"partner_id": bp.ID, "number": bp.Number, "partner_change_id": c.ID, "workflow_request_id": c.WorkflowRequestID,
			"status": Active, "legal_entity_ids": les}))
	}
	for _, b := range pl.BankAccounts {
		e, err := insertBank(ctx, tx, p, c, b, earliest)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, nil
}

func applyUpdate(ctx context.Context, tx pgx.Tx, p reqctx.Principal, c Change, bp Partner, day time.Time, w extensions.Writes) ([]outbox.Event, error) {
	var pl updatePayload
	decode(c.Payload, &pl)
	payload := map[string]any{"partner_id": bp.ID, "partner_change_id": c.ID}
	var fields []string
	name, legal, lang, ext := bp.Name, bp.LegalName, bp.Language, bp.Extensions
	if pl.Name != nil {
		name, payload["name"] = *pl.Name, *pl.Name
		fields = append(fields, "name")
	}
	if pl.LegalName.Set {
		legal, payload["legal_name"] = pl.LegalName.Value, pl.LegalName.Value
		fields = append(fields, "legal_name")
	}
	if pl.Language.Set {
		lang, payload["language"] = pl.Language.Value, pl.Language.Value
		fields = append(fields, "language")
	}
	if pl.Extensions != nil {
		ext = mergepatch.Apply(ext, pl.Extensions)
		fields = append(fields, "extensions")
	}
	// coalesce: a change requested before T58c carries no schema version and
	// keeps the partner's; schemas are never removed, so no other change has none.
	if _, err := tx.Exec(ctx, `UPDATE partner.business_partner SET name = $2, legal_name = $3, language = $4, extensions = $5,
		extensions_schema = coalesce($7, extensions_schema), version = version + 1, updated_by = $6 WHERE id = $1`,
		bp.ID, name, legal, lang, ext, p.ID, pl.ExtensionsSchema); err != nil {
		return nil, fmt.Errorf("partner: update business partner %s: %w", bp.ID, err)
	}
	if pl.Roles != nil {
		if err := addRoles(ctx, tx, p, bp.ID, pl.Roles); err != nil {
			return nil, err
		}
		payload["roles"] = pl.Roles
		fields = append(fields, "roles")
	}
	if err := putCompanies(ctx, tx, p, bp.ID, pl.SupplierCompanies); err != nil {
		return nil, err
	}
	for _, s := range pl.SupplierCompanies {
		fields = append(fields, "supplier_company."+s.LegalEntityID.String())
	}
	for i := range pl.Addresses {
		pl.Addresses[i].ValidFrom = Date{later(pl.Addresses[i].ValidFrom.Time, day)}
		fields = append(fields, "address."+pl.Addresses[i].Purpose)
	}
	if err := addAddresses(ctx, tx, p, bp.ID, pl.Addresses, true); err != nil {
		return nil, err
	}
	if err := putTaxes(ctx, tx, p, bp.ID, pl.TaxIdentifiers); err != nil {
		return nil, err
	}
	for _, t := range pl.TaxIdentifiers {
		fields = append(fields, "tax_identifier."+t.Scheme)
	}
	payload["changed_fields"] = fields
	return updated(ctx, w, bp, pl.ExtensionsSchema, ext, payload)
}

// updated is BusinessPartnerUpdated: the changed facts, and the partner's
// custom fields as the update leaves them, masked. The version is the
// change's, or, for a change requested before T58c, the partner's, as the
// update keeps it.
func updated(ctx context.Context, w extensions.Writes, bp Partner, version *int, ext, payload map[string]any) ([]outbox.Event, error) {
	if version == nil {
		version = bp.ExtensionsSchema
	}
	if err := w.Event(ctx, payload, ext, version); err != nil {
		return nil, err
	}
	return []outbox.Event{event("business_partner_updated", bp.ID, "partner.business_partner", payload)}, nil
}

func applyBankChange(ctx context.Context, tx pgx.Tx, p reqctx.Principal, c Change, day time.Time) ([]outbox.Event, error) {
	var pl bankChangePayload
	decode(c.Payload, &pl)
	if pl.Action == "add" {
		e, err := insertBank(ctx, tx, p, c, *pl.BankAccount, day)
		return []outbox.Event{e}, err
	}
	b, err := getBank(ctx, tx, c.PartnerID, *pl.BankAccountID)
	if err != nil {
		return nil, err
	}
	to := later(pl.ValidTo.Time, day)
	if b.ValidTo != nil && !to.Before(*b.ValidTo) {
		return nil, problem.Conflict("partner.invalid_end", "The bank account already ends on "+b.ValidTo.Format(time.DateOnly)+".")
	}
	if _, err := tx.Exec(ctx, `UPDATE partner.bank_account SET valid_to = $2, ended_by = $3 WHERE id = $1`, b.ID, to, p.ID); err != nil {
		return nil, fmt.Errorf("partner: end bank account %s of business partner %s: %w", b.ID, c.PartnerID, err)
	}
	return []outbox.Event{event("bank_account_changed", c.PartnerID, "partner.business_partner", map[string]any{
		"partner_id": c.PartnerID, "bank_account_id": nil, "ended_bank_account_id": b.ID, "ended_valid_to": to.Format(time.DateOnly), // no new account: the change only ends one
		"purpose": b.Purpose, "account_scheme": b.AccountScheme, "account_last4": b.AccountLast4, "bic": b.BIC,
		"institution_number": b.InstitutionNumber, "transit_number": b.TransitNumber,
		"bank_country_code": b.BankCountryCode, "valid_from": b.ValidFrom.Format(time.DateOnly), "valid_to": to.Format(time.DateOnly),
		"partner_change_id": c.ID})}, nil
}

func applyUnblock(ctx context.Context, tx pgx.Tx, p reqctx.Principal, c Change, bp Partner) ([]outbox.Event, error) {
	if bp.Status != Blocked {
		return nil, problem.Conflict("partner.invalid_status", "The business partner is "+bp.Status+"; unblocking requires status blocked.")
	}
	if _, err := tx.Exec(ctx, `UPDATE partner.business_partner SET status = 'active', version = version + 1, updated_by = $2 WHERE id = $1`,
		bp.ID, p.ID); err != nil {
		return nil, fmt.Errorf("partner: unblock business partner %s: %w", bp.ID, err)
	}
	return []outbox.Event{event("partner_unblocked", bp.ID, "partner.business_partner", map[string]any{
		"partner_id": bp.ID, "scope": "all", "legal_entity_id": nil, "partner_change_id": c.ID, "reason": c.Reason})}, nil
}

// RejectChange rejects a pending change; the workflow context calls it when
// the approval request is rejected. A rejected create leaves the partner
// pending.
func RejectChange(ctx context.Context, id uuid.UUID) (Change, error) {
	return closeChange(ctx, id, ChangeRejected, "reject", "partner_change_rejected")
}

// CancelChange withdraws a pending change at its requester's request.
func CancelChange(ctx context.Context, id uuid.UUID) (Change, error) {
	return closeChange(ctx, id, ChangeCancelled, "cancel", "partner_change_cancelled")
}

func closeChange(ctx context.Context, id uuid.UUID, status, action, name string) (Change, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Change{}, err
	}
	c, err := pendingChange(ctx, tx, id, action)
	if err != nil {
		return Change{}, err
	}
	closed, err := scanChange(tx.QueryRow(ctx, `UPDATE partner.partner_change SET status = $2, version = version + 1, updated_by = $3
		WHERE id = $1 RETURNING `+changeColumns, id, status, p.ID))
	if err != nil {
		return Change{}, fmt.Errorf("partner: %s partner change %s: %w", action, id, err)
	}
	return closed, audit.Record(ctx, "partner.change."+action, "partner.partner_change", id, map[string]any{"kind": c.Kind, "partner_id": c.PartnerID},
		event(name, id, "partner.partner_change", map[string]any{"partner_change_id": id, "number": c.Number, "kind": c.Kind,
			"partner_id": c.PartnerID, "workflow_request_id": c.WorkflowRequestID, "previous_status": ChangePending, "status": status}))
}
