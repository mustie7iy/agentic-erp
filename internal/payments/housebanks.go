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
// payments.house_bank with their IBANs sealed. A bank's name is its code
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
	Keys *fieldcrypt.Keyring
}

func init() {
	fieldcrypt.Register(fieldcrypt.Column{Table: "payments.house_bank", Keys: []string{"id"}, Cipher: "iban_cipher", Versioned: true})
}

var houseBankCode = regexp.MustCompile(`^[A-Z0-9]{1,5}$`)

// Check refuses names that are not house bank codes and new bank details
// under the name of an existing house bank.
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

// sameDetails reports whether the legal entity's house bank of that name,
// if there is one, has the bank details given.
func (h HouseBanks) sameDetails(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, le tenancy.Entity, hb config.HouseBank) (bool, error) {
	var sealed []byte
	var bic string
	err := tx.QueryRow(ctx, `SELECT iban_cipher, bic FROM payments.house_bank WHERE legal_entity_id = $1 AND code = $2`, le.ID, hb.Name).Scan(&sealed, &bic)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read: %w", err)
	}
	iban, err := h.Keys.Open(ctx, tenant, sealed)
	if err != nil {
		return false, err
	}
	return iban == fieldcrypt.Normalize(hb.IBAN) && bic == hb.BIC, nil
}

// Apply makes the house banks the document no longer lists inactive, then
// creates each listed one, or reactivates it and updates its ledger
// account. Deactivating first keeps an account that moves to a new bank
// from being active on two at once.
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
		if _, err := tx.Exec(ctx, `UPDATE payments.house_bank SET status = 'inactive', version = version + 1, updated_by = $3
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

// upsert creates the house bank active on the account, or reactivates it
// and moves it to the account.
func (h HouseBanks) upsert(ctx context.Context, tx pgx.Tx, p reqctx.Principal, le uuid.UUID, currency string, hb config.HouseBank, account uuid.UUID) error {
	sealed, err := h.Keys.Seal(ctx, p.TenantID, hb.IBAN)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO payments.house_bank AS h (tenant_id, id, legal_entity_id, code, name, iban_cipher, iban_last4, bic, currency,
			gl_account_id, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $4, $5, $6, $7, $8, $9, 'active', $10, $10)
		ON CONFLICT (tenant_id, legal_entity_id, code) DO UPDATE SET gl_account_id = EXCLUDED.gl_account_id, status = 'active',
			version = h.version + 1, updated_by = EXCLUDED.updated_by
		WHERE (h.gl_account_id, h.status) IS DISTINCT FROM (EXCLUDED.gl_account_id, 'active')`,
		p.TenantID, uuid.Must(uuid.NewV7()), le, hb.Name, sealed, fieldcrypt.Last4(hb.IBAN), hb.BIC, currency, account, p.ID)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}
