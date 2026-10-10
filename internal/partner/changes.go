package partner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/bankfile"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

var (
	kinds           = []string{"organization", "person"}
	roles           = []string{"supplier", "customer"}
	addressPurposes = []string{"legal", "remit_to", "order_from", "ship_from"}
	bankPurposes    = []string{"payments", "collections"}
	accountSchemes  = []string{"iban", "domestic"}
	accountNumber   = regexp.MustCompile(`^[A-Z0-9]{4,34}$`)
)

// SupplierCompanyInput is supplier data for one legal entity.
type SupplierCompanyInput struct {
	LegalEntityID           uuid.UUID `json:"legal_entity_id"`
	ReconciliationAccountID uuid.UUID `json:"reconciliation_account_id"`
	PaymentTermCode         string    `json:"payment_term_code"`
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
}

// nullable is a merge-patch value: set, possibly to null.
type nullable struct {
	Set   bool
	Value *string
}

func (n *nullable) UnmarshalJSON(b []byte) error {
	n.Set = true
	return json.Unmarshal(b, &n.Value)
}

func (n nullable) MarshalJSON() ([]byte, error) { return json.Marshal(n.Value) }

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
		return err
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
	seen := map[uuid.UUID]bool{}
	for i, s := range in {
		f := fmt.Sprintf("supplier_companies[%d]", i)
		if seen[s.LegalEntityID] {
			return invalid("partner.duplicate_company", f+".legal_entity_id", "one entry per legal entity")
		}
		seen[s.LegalEntityID] = true
		_, ok, err := ports.LegalEntities.Entity(ctx, s.LegalEntityID)
		if err != nil {
			return err
		}
		if !ok || !access.Allows(s.LegalEntityID) {
			return problem.Unprocessable("request.reference_not_found", "No legal entity with this id.",
				problem.FieldError{Field: f + ".legal_entity_id", Code: "not_found", Message: "no legal entity with this id that you can access"})
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
			return err
		}
		if !term {
			return invalid("partner.unknown_payment_term", f+".payment_term_code", "payment terms from the configuration")
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

func sealTaxes(keys *fieldcrypt.Keys, tenant uuid.UUID, in []TaxInput) ([]sealedTax, error) {
	out := []sealedTax{}
	seen := map[string]bool{}
	for i, t := range in {
		if seen[t.Scheme] {
			return nil, invalid("partner.duplicate_tax_scheme", fmt.Sprintf("tax_identifiers[%d].scheme", i), "one identifier per scheme")
		}
		seen[t.Scheme] = true
		out = append(out, sealedTax{Scheme: t.Scheme, ValueCipher: keys.Seal(tenant, t.Value),
			ValueFingerprint: keys.Fingerprint(tenant, t.Value), ValueLast4: fieldcrypt.Last4(t.Value)})
	}
	return out, nil
}

func sealBanks(keys *fieldcrypt.Keys, tenant uuid.UUID, day time.Time, in []BankInput, field func(int) string) ([]sealedBank, error) {
	out := []sealedBank{}
	seen := map[string]bool{}
	for i, b := range in {
		f := field(i)
		if b.Purpose == "" {
			b.Purpose = "payments"
		}
		number := fieldcrypt.Normalize(b.AccountNumber)
		switch {
		case !slices.Contains(bankPurposes, b.Purpose):
			return nil, invalid("partner.unknown_bank_purpose", f+"purpose", "payments or collections")
		case seen[b.Purpose]:
			return nil, invalid("partner.duplicate_bank_purpose", f+"purpose", "one new account per purpose")
		case !slices.Contains(accountSchemes, b.AccountScheme):
			return nil, invalid("partner.unknown_account_scheme", f+"account_scheme", "iban or domestic")
		case !accountNumber.MatchString(number):
			return nil, invalid("partner.invalid_account_number", f+"account_number", "4-34 letters and digits")
		case b.AccountScheme == "iban" && !bankfile.ValidIBAN(number):
			return nil, invalid("partner.invalid_iban", f+"account_number", "an IBAN that passes the ISO 13616 check")
		case b.ValidFrom.Before(day):
			return nil, invalid("partner.backdated_bank_account", f+"valid_from", "today or later")
		}
		seen[b.Purpose] = true
		out = append(out, sealedBank{Purpose: b.Purpose, AccountCipher: keys.Seal(tenant, number), AccountFingerprint: keys.Fingerprint(tenant, number),
			AccountLast4: fieldcrypt.Last4(number), AccountScheme: b.AccountScheme, BIC: b.BIC, BankCountryCode: b.BankCountryCode,
			HolderName: b.HolderName, ValidFrom: Date{b.ValidFrom}})
	}
	return out, nil
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
		return nil, err
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
	return scanChange(tx.QueryRow(ctx, `INSERT INTO partner.partner_change (tenant_id, id, number, kind, partner_id, payload, reason, warnings, status,
			requested_by, applied_at, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, CASE WHEN $11 THEN now() END, $10, $10) RETURNING `+changeColumns,
		p.TenantID, uuid.Must(uuid.NewV7()), number, kind, partnerID, toJSON(payload), reason, warns, status, p.ID, applied))
}

// The insert helpers write partner data from payloads, for creation and
// for applied changes.

func addRoles(ctx context.Context, tx pgx.Tx, p reqctx.Principal, partnerID uuid.UUID, rs []string) error {
	_, err := tx.Exec(ctx, `INSERT INTO partner.partner_role (tenant_id, partner_id, role, created_by)
		SELECT $1, $2, r, $3 FROM unnest($4::text[]) AS r ON CONFLICT DO NOTHING`, p.TenantID, partnerID, p.ID, rs)
	return err
}

func putCompanies(ctx context.Context, tx pgx.Tx, p reqctx.Principal, partnerID uuid.UUID, in []SupplierCompanyInput) error {
	var les, accounts []uuid.UUID
	var terms []string
	for _, s := range in {
		les, accounts, terms = append(les, s.LegalEntityID), append(accounts, s.ReconciliationAccountID), append(terms, s.PaymentTermCode)
	}
	_, err := tx.Exec(ctx, `INSERT INTO partner.supplier_company AS s (tenant_id, partner_id, legal_entity_id, reconciliation_account_id,
			payment_term_code, created_by, updated_by)
		SELECT $1, $2, le, a, t, $3, $3 FROM unnest($4::uuid[], $5::uuid[], $6::text[]) AS x(le, a, t)
		ON CONFLICT (tenant_id, partner_id, legal_entity_id) DO UPDATE SET reconciliation_account_id = EXCLUDED.reconciliation_account_id,
			payment_term_code = EXCLUDED.payment_term_code, version = s.version + 1, updated_by = $3`,
		p.TenantID, partnerID, p.ID, les, accounts, terms)
	return err
}

// addAddresses inserts addresses; with replace, each ends the current
// address of its purpose on its first day.
func addAddresses(ctx context.Context, tx pgx.Tx, p reqctx.Principal, partnerID uuid.UUID, in []AddressInput, replace bool) error {
	for _, a := range in {
		if replace {
			if _, err := tx.Exec(ctx, `UPDATE partner.address SET valid_to = $3, ended_by = $4
				WHERE partner_id = $1 AND purpose = $2 AND valid_from < $3 AND (valid_to IS NULL OR valid_to > $3)`,
				partnerID, a.Purpose, a.ValidFrom.Time, p.ID); err != nil {
				return err
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
			return err
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
	return err
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
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Partner{}, Change{}, err
	}
	if !slices.Contains(kinds, in.Kind) {
		return Partner{}, Change{}, invalid("partner.unknown_kind", "kind", "organization or person")
	}
	for i, r := range in.Roles {
		if !slices.Contains(roles, r) {
			return Partner{}, Change{}, invalid("partner.unknown_role", fmt.Sprintf("roles[%d]", i), "supplier or customer")
		}
	}
	if in.Extensions == nil {
		in.Extensions = map[string]any{}
	}
	if err := checkExtensions(in.Extensions); err != nil {
		return Partner{}, Change{}, err
	}
	day, err := today(ctx, tx)
	if err != nil {
		return Partner{}, Change{}, err
	}
	fields, codes := []string{"country_code"}, []string{in.CountryCode}
	for i, a := range in.Addresses {
		fields, codes = append(fields, fmt.Sprintf("addresses[%d].country_code", i)), append(codes, a.CountryCode)
	}
	for i, b := range in.BankAccounts {
		fields, codes = append(fields, fmt.Sprintf("bank_accounts[%d].bank_country_code", i)), append(codes, b.BankCountryCode)
	}
	if err := checkCountries(ctx, tx, fields, codes); err != nil {
		return Partner{}, Change{}, err
	}
	if err := checkCompanies(ctx, tx, ports, slices.Contains(in.Roles, "supplier"), in.SupplierCompanies); err != nil {
		return Partner{}, Change{}, err
	}
	if err := checkAddresses(day, in.Addresses, false); err != nil {
		return Partner{}, Change{}, err
	}
	taxes, err := sealTaxes(ports.Keys, p.TenantID, in.TaxIdentifiers)
	if err != nil {
		return Partner{}, Change{}, err
	}
	banks, err := sealBanks(ports.Keys, p.TenantID, day, in.BankAccounts, func(i int) string { return fmt.Sprintf("bank_accounts[%d].", i) })
	if err != nil {
		return Partner{}, Change{}, err
	}
	number, err := nextNumber(ctx, tx, p.TenantID, "business_partner", "BP")
	if err != nil {
		return Partner{}, Change{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, legal_name, country_code, language,
			status, extensions, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending', $9, $10, $10)`,
		p.TenantID, id, number, in.Kind, in.Name, in.LegalName, in.CountryCode, in.Language, in.Extensions, p.ID); err != nil {
		return Partner{}, Change{}, err
	}
	if err := addRoles(ctx, tx, p, id, in.Roles); err != nil {
		return Partner{}, Change{}, err
	}
	if err := putCompanies(ctx, tx, p, id, in.SupplierCompanies); err != nil {
		return Partner{}, Change{}, err
	}
	if err := addAddresses(ctx, tx, p, id, in.Addresses, false); err != nil {
		return Partner{}, Change{}, err
	}
	if err := putTaxes(ctx, tx, p, id, taxes); err != nil {
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
	return bp, c, record(ctx, "partner.business_partner.create", "partner.business_partner", id,
		map[string]any{"number": number, "partner_change_id": c.ID, "warnings": len(warns)},
		event("business_partner_created", id, "partner.business_partner", map[string]any{
			"partner_id": id, "number": number, "kind": in.Kind, "name": in.Name, "country_code": in.CountryCode, "roles": bp.Roles,
			"status": Pending, "partner_change_id": c.ID}))
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
		return Change{}, err
	}
	c, err := insertChange(ctx, tx, p, KindBlock, id, map[string]any{"scope": "all"}, reason, nil, true)
	if err != nil {
		return Change{}, err
	}
	return c, record(ctx, "partner.business_partner.block", "partner.business_partner", id, map[string]any{"partner_change_id": c.ID, "reason": reason},
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
	return c, record(ctx, "partner.change.request", "partner.partner_change", c.ID, map[string]any{"kind": c.Kind, "partner_id": id})
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
		if problem.From(err).Status == 404 {
			return Change{}, problem.Unprocessable("request.reference_not_found", "No business partner with this id.",
				problem.FieldError{Field: "partner_id", Code: "not_found", Message: "no business partner with this id"})
		}
		return Change{}, err
	}
	if ch.Name == nil && ch.LegalName == nil && ch.Language == nil && ch.Roles == nil && len(ch.SupplierCompanies) == 0 &&
		len(ch.Addresses) == 0 && len(ch.TaxIdentifiers) == 0 && len(ch.Extensions) == 0 {
		return Change{}, problem.Unprocessable("partner.no_changes", "The change requests no changes.")
	}
	all := bp.Roles
	if ch.Roles != nil {
		for i, r := range ch.Roles {
			if !slices.Contains(roles, r) {
				return Change{}, invalid("partner.unknown_role", fmt.Sprintf("payload.roles[%d]", i), "supplier or customer")
			}
		}
		for _, r := range bp.Roles {
			if !slices.Contains(ch.Roles, r) {
				return Change{}, invalid("partner.role_removal", "payload.roles", "roles can be added but not removed; "+r+" is missing")
			}
		}
		all = ch.Roles
	}
	if err := checkExtensions(ch.Extensions); err != nil {
		return Change{}, err
	}
	day, err := today(ctx, tx)
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
	taxes, err := sealTaxes(ports.Keys, p.TenantID, ch.TaxIdentifiers)
	if err != nil {
		return Change{}, err
	}
	warns, err := warnings(ctx, tx, partnerID, taxes, nil)
	if err != nil {
		return Change{}, err
	}
	c, err := insertChange(ctx, tx, p, KindUpdate, partnerID, updatePayload{Name: ch.Name, LegalName: toNullable(ch.LegalName),
		Language: toNullable(ch.Language), Roles: ch.Roles, SupplierCompanies: ch.SupplierCompanies, Addresses: ch.Addresses,
		TaxIdentifiers: taxes, Extensions: ch.Extensions}, reason, warns, false)
	if err != nil {
		return Change{}, err
	}
	return c, record(ctx, "partner.change.request", "partner.partner_change", c.ID, map[string]any{"kind": c.Kind, "partner_id": partnerID})
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
	day, err := today(ctx, tx)
	if err != nil {
		return Change{}, err
	}
	if err := checkCountries(ctx, tx, []string{"bank_country_code"}, []string{in.BankCountryCode}); err != nil {
		return Change{}, err
	}
	banks, err := sealBanks(ports.Keys, p.TenantID, day, []BankInput{in}, func(int) string { return "" })
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
	return c, record(ctx, "partner.change.request", "partner.partner_change", c.ID, map[string]any{"kind": c.Kind, "partner_id": partnerID, "action": "add"})
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
	day, err := today(ctx, tx)
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
	return c, record(ctx, "partner.change.request", "partner.partner_change", c.ID,
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
// calls it when the change's approval request is approved (T4).
func ApplyChange(ctx context.Context, id uuid.UUID) (Change, error) {
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
	day, err := today(ctx, tx)
	if err != nil {
		return Change{}, err
	}
	var events []outbox.Event
	switch c.Kind {
	case KindCreate:
		events, err = applyCreate(ctx, tx, p, c, bp, day)
	case KindUpdate:
		events, err = applyUpdate(ctx, tx, p, c, bp, day)
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
		return Change{}, err
	}
	return applied, record(ctx, "partner.change.apply", "partner.partner_change", id, map[string]any{"kind": c.Kind, "partner_id": c.PartnerID}, events...)
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// insertBank adds an approved account, ending the current account of its
// purpose on the new account's first day.
func insertBank(ctx context.Context, tx pgx.Tx, p reqctx.Principal, c Change, b sealedBank, day time.Time) (outbox.Event, error) {
	from := later(b.ValidFrom.Time, day)
	var ended *uuid.UUID
	var endedTo *time.Time
	err := tx.QueryRow(ctx, `UPDATE partner.bank_account SET valid_to = $3, ended_by = $4
		WHERE partner_id = $1 AND purpose = $2 AND valid_from < $3 AND (valid_to IS NULL OR valid_to > $3) RETURNING id, valid_to`,
		c.PartnerID, b.Purpose, from, p.ID).Scan(&ended, &endedTo)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return outbox.Event{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO partner.bank_account (tenant_id, id, partner_id, purpose, account_cipher, account_fingerprint,
			account_last4, account_scheme, bic, bank_country_code, holder_name, valid_from, partner_change_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		p.TenantID, id, c.PartnerID, b.Purpose, b.AccountCipher, b.AccountFingerprint, b.AccountLast4, b.AccountScheme, b.BIC,
		b.BankCountryCode, b.HolderName, from, c.ID, p.ID); err != nil {
		return outbox.Event{}, err
	}
	return event("bank_account_changed", c.PartnerID, "partner.business_partner", map[string]any{
		"partner_id": c.PartnerID, "bank_account_id": id, "ended_bank_account_id": ended, "ended_valid_to": dateOrNil(endedTo),
		"purpose": b.Purpose, "account_scheme": b.AccountScheme, "account_last4": b.AccountLast4, "bic": b.BIC,
		"bank_country_code": b.BankCountryCode, "valid_from": from.Format(time.DateOnly), "valid_to": nil, "partner_change_id": c.ID}), nil
}

func dateOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format(time.DateOnly)
}

func applyCreate(ctx context.Context, tx pgx.Tx, p reqctx.Principal, c Change, bp Partner, day time.Time) ([]outbox.Event, error) {
	var pl createPayload
	decode(c.Payload, &pl)
	if _, err := tx.Exec(ctx, `UPDATE partner.business_partner SET status = 'active', version = version + 1, updated_by = $2 WHERE id = $1`,
		bp.ID, p.ID); err != nil {
		return nil, err
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
		e, err := insertBank(ctx, tx, p, c, b, day)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, nil
}

func applyUpdate(ctx context.Context, tx pgx.Tx, p reqctx.Principal, c Change, bp Partner, day time.Time) ([]outbox.Event, error) {
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
		ext = mergePatch(ext, pl.Extensions)
		fields = append(fields, "extensions")
	}
	if _, err := tx.Exec(ctx, `UPDATE partner.business_partner SET name = $2, legal_name = $3, language = $4, extensions = $5,
		version = version + 1, updated_by = $6 WHERE id = $1`, bp.ID, name, legal, lang, ext, p.ID); err != nil {
		return nil, err
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
		return nil, err
	}
	return []outbox.Event{event("bank_account_changed", c.PartnerID, "partner.business_partner", map[string]any{
		"partner_id": c.PartnerID, "bank_account_id": b.ID, "ended_bank_account_id": b.ID, "ended_valid_to": to.Format(time.DateOnly),
		"purpose": b.Purpose, "account_scheme": b.AccountScheme, "account_last4": b.AccountLast4, "bic": b.BIC,
		"bank_country_code": b.BankCountryCode, "valid_from": b.ValidFrom.Format(time.DateOnly), "valid_to": to.Format(time.DateOnly),
		"partner_change_id": c.ID})}, nil
}

func applyUnblock(ctx context.Context, tx pgx.Tx, p reqctx.Principal, c Change, bp Partner) ([]outbox.Event, error) {
	if bp.Status != Blocked {
		return nil, problem.Conflict("partner.invalid_status", "The business partner is "+bp.Status+"; unblocking requires status blocked.")
	}
	if _, err := tx.Exec(ctx, `UPDATE partner.business_partner SET status = 'active', version = version + 1, updated_by = $2 WHERE id = $1`,
		bp.ID, p.ID); err != nil {
		return nil, err
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
		return Change{}, err
	}
	return closed, record(ctx, "partner.change."+action, "partner.partner_change", id, map[string]any{"kind": c.Kind, "partner_id": c.PartnerID},
		event(name, id, "partner.partner_change", map[string]any{"partner_change_id": id, "number": c.Number, "kind": c.Kind,
			"partner_id": c.PartnerID, "workflow_request_id": c.WorkflowRequestID, "previous_status": ChangePending, "status": status}))
}
