package payments

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// HouseBanks is the configuration document's house banks (SAP T012K): each
// company's main house_bank and further house_banks, kept in
// payments.house_bank with their IBANs, or a Canadian bank's account
// number beside its institution and transit numbers (T45d), sealed. A
// bank's name is its code within the legal entity, its currency its own or
// the company's (ADR-0051 §3, T53a) and its ledger account its own bank
// clearing account, or for a main bank without one the account
// determination's bank account (ADR-0038). An IBAN, BIC or currency never
// changes under the same name, so files already produced stay reproducible
// and a bank's statements and balances stay in one currency: a new bank
// account gets a new name, and a house bank the document no longer lists
// becomes inactive.
type HouseBanks struct {
	LegalEntities interface {
		EntityByCode(ctx context.Context, code string) (tenancy.Entity, bool, error)
	}
	Accounts interface {
		Chart(ctx context.Context, legalEntity uuid.UUID) (map[string]ledger.ChartAccount, error)
	}
	// Channels finds the bank connector a house bank names (integration
	// context, through the application's wiring; T44b).
	Channels interface {
		ChannelByName(ctx context.Context, name string) (uuid.UUID, bool, error)
	}
	Keys *fieldcrypt.Keyring
}

func init() {
	fieldcrypt.Register(fieldcrypt.Column{Table: "payments.house_bank", Keys: []string{"id"}, Cipher: "iban_cipher", Versioned: true})
}

var houseBankCode = regexp.MustCompile(`^[A-Z0-9]{1,5}$`)

// Check refuses names that are not house bank codes, new bank details or
// another currency under the name of an existing house bank, and a
// connector that is not a bank channel.
func (h HouseBanks) Check(ctx context.Context, _, proposed *config.Config) ([]config.Finding, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	var out []config.Finding
	for _, co := range proposed.Companies {
		findings, err := h.checkCompany(ctx, tx, p.TenantID, &co)
		if err != nil {
			return nil, err
		}
		out = append(out, findings...)
	}
	return out, nil
}

// checkCompany checks the company's house banks.
func (h HouseBanks) checkCompany(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, co *config.Company) ([]config.Finding, error) {
	le, ok, err := h.LegalEntities.EntityByCode(ctx, co.Code)
	if err != nil {
		return nil, fmt.Errorf("payments: legal entity %s: %w", co.Code, err)
	}
	var out []config.Finding
	for i, hb := range co.AllHouseBanks() {
		path := "companies[" + co.Code + "].house_bank"
		if i > 0 {
			path = "companies[" + co.Code + "].house_banks[" + hb.Name + "]"
		}
		if !houseBankCode.MatchString(hb.Name) {
			out = append(out, config.Finding{Severity: config.SevError, Path: path + ".name", Message: "a house bank's name is its code: 1-5 upper-case letters or digits"})
			continue
		}
		channel, err := h.checkChannel(ctx, path, hb)
		if err != nil {
			return nil, err
		}
		out = append(out, channel...)
		if !ok { // the ledger section reports companies without a legal entity
			continue
		}
		changed, err := h.changed(ctx, tx, tenant, le, co.HouseBankCurrency(hb), hb)
		if err != nil {
			return nil, fmt.Errorf("payments: house bank %s of legal entity %s: %w", hb.Name, co.Code, err)
		}
		if changed != "" {
			out = append(out, config.Finding{Severity: config.SevError, Path: path,
				Message: fmt.Sprintf("house bank %s already has %s; give the new bank account a new name", hb.Name, changed)})
		}
	}
	return out, nil
}

// checkChannel finds the bank channel the house bank names, if it names one.
func (h HouseBanks) checkChannel(ctx context.Context, path string, hb config.HouseBank) ([]config.Finding, error) {
	if hb.Connector == "" {
		return nil, nil
	}
	if _, found, err := h.Channels.ChannelByName(ctx, hb.Connector); err != nil || found {
		return nil, err
	}
	return []config.Finding{{Severity: config.SevError, Path: path + ".connector",
		Message: fmt.Sprintf("no bank channel connector (bank_sftp) is named %s", hb.Connector)}}, nil
}

// changed says what the legal entity's house bank of that name, if there
// is one, has other than the bank details and currency given: "" nothing,
// else "other bank details" or "another currency, <code>".
func (h HouseBanks) changed(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, le tenancy.Entity, currency string, hb config.HouseBank) (string, error) {
	var sealed []byte
	var bic, institution, transit, was string
	err := tx.QueryRow(ctx, `SELECT iban_cipher, bic, coalesce(institution_number, ''), coalesce(transit_number, ''), currency
		FROM payments.house_bank WHERE legal_entity_id = $1 AND code = $2`, le.ID, hb.Name).Scan(&sealed, &bic, &institution, &transit, &was)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	account, err := h.Keys.Open(ctx, tenant, sealed)
	if err != nil {
		return "", err
	}
	wantInstitution, wantTransit := hb.Routing()
	switch {
	case account != fieldcrypt.Normalize(hb.Account()) || bic != hb.BIC || institution != wantInstitution || transit != wantTransit:
		return "other bank details", nil
	case was != currency:
		return "another currency, " + was, nil
	}
	return "", nil
}

// Apply makes the house banks the document no longer lists inactive, then
// creates each listed one, or reactivates it and updates its ledger
// account. Deactivating first keeps an account that moves to a new bank
// from being active on two at once; house_bank_account_once (000098) is
// checked at commit, so banks that swap accounts pass too.
func (h HouseBanks) Apply(ctx context.Context, doc *config.Config) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	for _, co := range doc.Companies {
		le, ok, err := h.LegalEntities.EntityByCode(ctx, co.Code)
		if err != nil {
			return err
		}
		if !ok { // checked on submission; legal entities are never deleted
			return fmt.Errorf("payments: no legal entity %s", co.Code)
		}
		chart, err := h.Accounts.Chart(ctx, le.ID)
		if err != nil {
			return err
		}
		banks := co.AllHouseBanks()
		codes := make([]string, len(banks))
		for i, hb := range banks {
			codes[i] = hb.Name
		}
		if _, err := tx.Exec(ctx, `UPDATE payments.house_bank SET status = 'inactive', active_gl_account_id = NULL, version = version + 1, updated_by = $3
			WHERE legal_entity_id = $1 AND code <> ALL($2) AND status = 'active'`, le.ID, codes, p.ID); err != nil {
			return fmt.Errorf("payments: deactivate the unlisted house banks of legal entity %s: %w", co.Code, err)
		}
		for _, hb := range banks {
			accounts, err := bankAccounts(chart, &co, hb)
			if err != nil {
				return err
			}
			if err := h.upsert(ctx, tx, p, le.ID, co.HouseBankCurrency(hb), hb, accounts); err != nil {
				return fmt.Errorf("payments: apply house bank %s of legal entity %s: %w", hb.Name, co.Code, err)
			}
		}
	}
	return nil
}

// houseBankAccounts are a house bank's ledger accounts: its clearing
// account and, for a bank whose statements post, its main bank account
// and incoming payments clearing account (ADR-0047).
type houseBankAccounts struct {
	clearing       uuid.UUID
	main, incoming *uuid.UUID
}

// bankAccounts finds the house bank's accounts in the company's chart.
func bankAccounts(chart map[string]ledger.ChartAccount, co *config.Company, hb config.HouseBank) (houseBankAccounts, error) {
	var out houseBankAccounts
	for _, a := range []struct {
		number string
		id     **uuid.UUID
	}{{co.HouseBankAccount(hb), nil}, {hb.MainGLAccount, &out.main}, {hb.IncomingGLAccount, &out.incoming}} {
		if a.number == "" {
			continue
		}
		account, ok := chart[a.number]
		if !ok { // the document requires the account, which the ledger section applied
			return out, fmt.Errorf("payments: legal entity %s has no account %s for house bank %s", co.Code, a.number, hb.Name)
		}
		if a.id == nil {
			out.clearing = account.ID
			continue
		}
		*a.id = &account.ID
	}
	return out, nil
}

// upsert creates the house bank active on the accounts and channel in its
// currency, or reactivates it and moves it to them; its currency, as its
// account, never changes (Check refuses another).
func (h HouseBanks) upsert(ctx context.Context, tx pgx.Tx, p reqctx.Principal, le uuid.UUID, currency string, hb config.HouseBank, accounts houseBankAccounts) error {
	sealed, err := h.Keys.Seal(ctx, p.TenantID, hb.Account())
	if err != nil {
		return err
	}
	institution, transit := hb.Routing()
	var channel *uuid.UUID
	if hb.Connector != "" {
		id, found, err := h.Channels.ChannelByName(ctx, hb.Connector)
		if err != nil {
			return err
		}
		if !found { // checked on submission; a connector renamed since names none
			return fmt.Errorf("no bank channel connector is named %s", hb.Connector)
		}
		channel = &id
	}
	_, err = tx.Exec(ctx, `INSERT INTO payments.house_bank AS h (tenant_id, id, legal_entity_id, code, name, iban_cipher, iban_last4, bic, currency,
			gl_account_id, active_gl_account_id, status, created_by, updated_by, connector_id, institution_number, transit_number,
			main_gl_account_id, incoming_gl_account_id)
		VALUES ($1, $2, $3, $4, $4, $5, $6, $7, $8, $9, $9, 'active', $10, $10, $11, NULLIF($12, ''), NULLIF($13, ''), $14, $15)
		ON CONFLICT (tenant_id, legal_entity_id, code) DO UPDATE SET gl_account_id = EXCLUDED.gl_account_id, active_gl_account_id = EXCLUDED.gl_account_id, status = 'active',
			connector_id = EXCLUDED.connector_id, main_gl_account_id = EXCLUDED.main_gl_account_id, incoming_gl_account_id = EXCLUDED.incoming_gl_account_id,
			version = h.version + 1, updated_by = EXCLUDED.updated_by
		WHERE (h.gl_account_id, h.status, h.connector_id, h.main_gl_account_id, h.incoming_gl_account_id)
			IS DISTINCT FROM (EXCLUDED.gl_account_id, 'active', EXCLUDED.connector_id, EXCLUDED.main_gl_account_id, EXCLUDED.incoming_gl_account_id)`,
		p.TenantID, uuid.Must(uuid.NewV7()), le, hb.Name, sealed, fieldcrypt.Last4(hb.Account()), hb.BIC, currency, accounts.clearing, p.ID, channel,
		institution, transit, accounts.main, accounts.incoming)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}
