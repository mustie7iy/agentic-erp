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
// number beside its institution and transit numbers (T45d), sealed. A bank's name is its code
// within the legal entity, its currency the company's and its ledger
// account its own bank clearing account, or for a main bank without one the
// account determination's bank account (ADR-0038). An IBAN or BIC never
// changes under the same name, so files already produced stay
// reproducible: a new bank account gets a new name, and a house bank the
// document no longer lists becomes inactive.
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

// Check refuses names that are not house bank codes, new bank details
// under the name of an existing house bank, and a connector that is not a
// bank channel.
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
		same, err := h.sameDetails(ctx, tx, tenant, le, hb)
		if err != nil {
			return nil, fmt.Errorf("payments: house bank %s of legal entity %s: %w", hb.Name, co.Code, err)
		}
		if !same {
			out = append(out, config.Finding{Severity: config.SevError, Path: path,
				Message: fmt.Sprintf("house bank %s already has other bank details; give the new bank account a new name", hb.Name)})
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

// sameDetails reports whether the legal entity's house bank of that name,
// if there is one, has the bank details given.
func (h HouseBanks) sameDetails(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, le tenancy.Entity, hb config.HouseBank) (bool, error) {
	var sealed []byte
	var bic, institution, transit string
	err := tx.QueryRow(ctx, `SELECT iban_cipher, bic, coalesce(institution_number, ''), coalesce(transit_number, '')
		FROM payments.house_bank WHERE legal_entity_id = $1 AND code = $2`, le.ID, hb.Name).Scan(&sealed, &bic, &institution, &transit)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read: %w", err)
	}
	account, err := h.Keys.Open(ctx, tenant, sealed)
	if err != nil {
		return false, err
	}
	wantInstitution, wantTransit := hb.Routing()
	return account == fieldcrypt.Normalize(hb.Account()) && bic == hb.BIC && institution == wantInstitution && transit == wantTransit, nil
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
			account, ok := chart[co.HouseBankAccount(hb)]
			if !ok { // the document requires the account, which the ledger section applied
				return fmt.Errorf("payments: legal entity %s has no account %s for house bank %s", co.Code, co.HouseBankAccount(hb), hb.Name)
			}
			if err := h.upsert(ctx, tx, p, le.ID, co.Currency, hb, account.ID); err != nil {
				return fmt.Errorf("payments: apply house bank %s of legal entity %s: %w", hb.Name, co.Code, err)
			}
		}
	}
	return nil
}

// upsert creates the house bank active on the account and channel, or
// reactivates it and moves it to them.
func (h HouseBanks) upsert(ctx context.Context, tx pgx.Tx, p reqctx.Principal, le uuid.UUID, currency string, hb config.HouseBank, account uuid.UUID) error {
	sealed, err := h.Keys.Seal(ctx, p.TenantID, hb.Account())
	institution, transit := hb.Routing()
	if err != nil {
		return err
	}
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
			gl_account_id, active_gl_account_id, status, created_by, updated_by, connector_id, institution_number, transit_number)
		VALUES ($1, $2, $3, $4, $4, $5, $6, $7, $8, $9, $9, 'active', $10, $10, $11, NULLIF($12, ''), NULLIF($13, ''))
		ON CONFLICT (tenant_id, legal_entity_id, code) DO UPDATE SET gl_account_id = EXCLUDED.gl_account_id, active_gl_account_id = EXCLUDED.gl_account_id, status = 'active',
			connector_id = EXCLUDED.connector_id, version = h.version + 1, updated_by = EXCLUDED.updated_by
		WHERE (h.gl_account_id, h.status, h.connector_id) IS DISTINCT FROM (EXCLUDED.gl_account_id, 'active', EXCLUDED.connector_id)`,
		p.TenantID, uuid.Must(uuid.NewV7()), le, hb.Name, sealed, fieldcrypt.Last4(hb.Account()), hb.BIC, currency, account, p.ID, channel,
		institution, transit)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}
