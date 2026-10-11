package payments

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// HouseBanks is the configuration document's house banks (SAP T012K): each
// company's house_bank, kept in payments.house_bank with its IBAN sealed.
// The bank's name is its code within the legal entity, its currency the
// company's and its ledger account the account determination's bank
// account. An IBAN or BIC never changes under the same name, so files
// already produced stay reproducible: a new bank account gets a new name,
// and the house bank it replaces becomes inactive.
type HouseBanks struct {
	LegalEntities interface {
		EntityByCode(ctx context.Context, code string) (tenancy.Entity, bool, error)
	}
	Accounts interface {
		Determination(ctx context.Context, legalEntity uuid.UUID, key string) (uuid.UUID, bool, error)
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
		path := "companies[" + co.Code + "].house_bank"
		hb := co.HouseBank
		if !houseBankCode.MatchString(hb.Name) {
			out = append(out, config.Finding{Severity: config.SevError, Path: path + ".name", Message: "a house bank's name is its code: 1-5 upper-case letters or digits"})
			continue
		}
		le, ok, err := h.LegalEntities.EntityByCode(ctx, co.Code)
		if err != nil {
			return nil, err
		}
		if !ok { // the ledger section reports companies without a legal entity
			continue
		}
		var sealed []byte
		var bic string
		err = tx.QueryRow(ctx, `SELECT iban_cipher, bic FROM payments.house_bank WHERE legal_entity_id = $1 AND code = $2`, le.ID, hb.Name).Scan(&sealed, &bic)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		iban, err := h.Keys.Open(ctx, p.TenantID, sealed)
		if err != nil {
			return nil, err
		}
		if iban != fieldcrypt.Normalize(hb.IBAN) || bic != hb.BIC {
			out = append(out, config.Finding{Severity: config.SevError, Path: path,
				Message: fmt.Sprintf("house bank %s already has other bank details; give the new bank account a new name", hb.Name)})
		}
	}
	return out, nil
}

// Apply creates each company's house bank, or reactivates it and updates
// its ledger account, and makes the legal entity's other house banks
// inactive.
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
		account, ok, err := h.Accounts.Determination(ctx, le.ID, "bank")
		if err != nil {
			return err
		}
		if !ok { // the document requires a bank account, which the ledger section applied
			return fmt.Errorf("payments: legal entity %s has no bank account", co.Code)
		}
		hb := co.HouseBank
		sealed, err := h.Keys.Seal(ctx, p.TenantID, hb.IBAN)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO payments.house_bank AS h (tenant_id, id, legal_entity_id, code, name, iban_cipher, iban_last4, bic, currency,
				gl_account_id, status, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $4, $5, $6, $7, $8, $9, 'active', $10, $10)
			ON CONFLICT (tenant_id, legal_entity_id, code) DO UPDATE SET gl_account_id = EXCLUDED.gl_account_id, status = 'active',
				version = h.version + 1, updated_by = EXCLUDED.updated_by
			WHERE (h.gl_account_id, h.status) IS DISTINCT FROM (EXCLUDED.gl_account_id, 'active')`,
			p.TenantID, uuid.Must(uuid.NewV7()), le.ID, hb.Name, sealed, fieldcrypt.Last4(hb.IBAN), hb.BIC,
			co.Currency, account, p.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE payments.house_bank SET status = 'inactive', version = version + 1, updated_by = $3
			WHERE legal_entity_id = $1 AND code <> $2 AND status = 'active'`, le.ID, hb.Name, p.ID); err != nil {
			return err
		}
	}
	return nil
}
