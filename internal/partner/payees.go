package partner

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Payees answers the payments context's questions about the suppliers it
// pays, in the request's transaction. Bank details are opened only to
// write them into a bank file.
type Payees struct {
	Keys *fieldcrypt.Keyring
}

// Payee is what a payment run needs to know about a supplier of a legal
// entity on the run date.
type Payee struct {
	Status         string // business partner status
	PostingBlocked bool
	PaymentBlocked bool
	NetDays        int // of the supplier's payment terms in the legal entity
	// BankAccountID is the payments bank account valid on the date, or nil.
	BankAccountID *uuid.UUID
	BankValidFrom time.Time
	// BankChanged is true when that account came from a bank change after
	// the partner was created, which starts the payment hold.
	BankChanged       bool
	BankChangePending bool
	// PaymentMethods are the methods it may be paid by in the legal entity
	// (SAP LFB1-ZWELS; ADR-0038): they restrict the methods a payment run
	// tries in the company's order, and their own order decides nothing.
	PaymentMethods []string
	// PlanningGroup is its planning group in the legal entity, "" for none
	// (ADR-0051 §4): what the liquidity forecast classifies its payments by
	// when their documents' lines do not (T53e3).
	PlanningGroup string
}

// Payees returns the partners that are suppliers set up in the legal
// entity, keyed by id; others are left out.
func (Payees) Payees(ctx context.Context, legalEntity uuid.UUID, partners []uuid.UUID, on time.Time) (map[uuid.UUID]Payee, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT p.id, p.status, sc.is_posting_blocked, sc.is_payment_blocked, t.net_days, b.id, b.valid_from,
			coalesce(c.kind <> 'create', false),
			EXISTS (SELECT FROM partner.partner_change pc WHERE pc.tenant_id = p.tenant_id AND pc.partner_id = p.id AND pc.kind = 'bank_change' AND pc.status = 'pending'),
			sc.payment_methods, coalesce(sc.planning_group, '')
		FROM partner.business_partner p
		JOIN partner.supplier_company sc ON sc.tenant_id = p.tenant_id AND sc.partner_id = p.id AND sc.legal_entity_id = $2
		JOIN partner.payment_term t ON t.tenant_id = sc.tenant_id AND t.code = sc.payment_term_code
		LEFT JOIN partner.bank_account b ON b.tenant_id = p.tenant_id AND b.partner_id = p.id AND b.purpose = 'payments'
			AND b.valid_from <= $3 AND (b.valid_to IS NULL OR b.valid_to > $3)
		LEFT JOIN partner.partner_change c ON c.tenant_id = b.tenant_id AND c.id = b.partner_change_id
		WHERE p.id = ANY($1)`, partners, legalEntity, on)
	out := map[uuid.UUID]Payee{}
	var id uuid.UUID
	var p Payee
	var validFrom *time.Time
	_, err = pgx.ForEachRow(rows, []any{&id, &p.Status, &p.PostingBlocked, &p.PaymentBlocked, &p.NetDays, &p.BankAccountID, &validFrom,
		&p.BankChanged, &p.BankChangePending, &p.PaymentMethods, &p.PlanningGroup}, func() error {
		if validFrom != nil {
			p.BankValidFrom = *validFrom
		}
		out[id] = p
		return nil
	})
	if err != nil {
		return out, fmt.Errorf("partner: read the payees of legal entity %s: %w", legalEntity, err)
	}
	return out, nil
}

// BankDetail is a bank account opened for a payment instruction.
type BankDetail struct {
	Holder  string
	Account string // IBAN, or the domestic or Canadian account number
	BIC     string
	// Institution and Transit are a Canadian account's bank (scheme ca,
	// T45d); empty for the other schemes.
	Institution, Transit string
}

// BankDetails opens the bank accounts, keyed by id.
func (y Payees) BankDetails(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]BankDetail, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT id, holder_name, account_cipher, coalesce(bic, ''), coalesce(institution_number, ''), coalesce(transit_number, '')
		FROM partner.bank_account WHERE id = ANY($1)`, ids)
	out := map[uuid.UUID]BankDetail{}
	var id uuid.UUID
	var d BankDetail
	var sealed []byte
	_, err = pgx.ForEachRow(rows, []any{&id, &d.Holder, &sealed, &d.BIC, &d.Institution, &d.Transit}, func() error {
		var err error
		if d.Account, err = y.Keys.Open(ctx, p.TenantID, sealed); err != nil {
			return err
		}
		out[id] = d
		return nil
	})
	if err != nil {
		return out, fmt.Errorf("partner: open %d bank accounts: %w", len(ids), err)
	}
	return out, nil
}

// Named is what a partner is known by: its number and its name.
type Named struct{ Number, Name string }

// Names returns the partners' numbers and names, keyed by id: what a
// payment shows beside its supplier's id, and how a refusal names the
// supplier. An id that names no partner is left out. The name of a
// partner whose personal data is blocked is MaskedText to a caller who
// may not read it (ADR-0079 §11): the number still names it.
func (Payees) Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Named, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT id, number, name, personal_data_blocked_at IS NOT NULL AND personal_data_deleted_at IS NULL
		FROM partner.business_partner WHERE id = ANY($1)`, ids)
	out := map[uuid.UUID]Named{}
	var blocked []uuid.UUID
	var id uuid.UUID
	var n Named
	var b bool
	_, err = pgx.ForEachRow(rows, []any{&id, &n.Number, &n.Name, &b}, func() error {
		out[id] = n
		if b {
			blocked = append(blocked, id)
		}
		return nil
	})
	if err != nil {
		return out, fmt.Errorf("partner: names of %d partners: %w", len(ids), err)
	}
	full, err := readsBlocked(ctx, len(blocked) > 0)
	if err != nil || full {
		return out, err
	}
	for _, id := range blocked {
		out[id] = Named{Number: out[id].Number, Name: MaskedText}
	}
	return out, nil
}

// BankAccounts returns the bank accounts, masked, keyed by id: what a
// payment shows beside its bank account's id. The account number stays
// sealed (BankDetails opens it, only for a bank file). An id that names no
// account is left out. An account of a partner whose personal data is
// blocked has its holder and identifiers masked too to a caller who may
// not read them (ADR-0079 §11).
func (Payees) BankAccounts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]BankAccount, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT `+bankColumns+` FROM partner.bank_account WHERE id = ANY($1)`, ids)
	accounts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (BankAccount, error) { return scanBank(row) })
	if err != nil {
		return nil, fmt.Errorf("partner: bank accounts of %d ids: %w", len(ids), err)
	}
	partners := make([]uuid.UUID, len(accounts))
	for i, a := range accounts {
		partners[i] = a.PartnerID
	}
	blocked, err := blockedAmong(ctx, tx, partners)
	if err != nil {
		return nil, err
	}
	full, err := readsBlocked(ctx, len(blocked) > 0)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]BankAccount, len(accounts))
	for _, a := range accounts {
		if !full && blocked[a.PartnerID] {
			a = maskedBank(a)
		}
		out[a.ID] = a
	}
	return out, nil
}

// Found is a business partner a document names: its id, number and name.
type Found struct {
	ID           uuid.UUID
	Number, Name string
}

// FindCustomers are the business partners a document names as its
// customer (T90a, order capture), each once, by number: the one with the
// number printed and those with a tax registration number of the value
// printed, whatever its scheme, found through the identifiers' keyed hash
// without opening one. An empty number or value finds none, nor does a
// partner whose creation waits for approval; what the partners are to the
// company is the caller's to check.
func (p Payees) FindCustomers(ctx context.Context, number, taxNumber string) ([]Found, error) {
	tx, pr, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	var print *string
	if taxNumber != "" {
		f, err := p.Keys.Fingerprint(ctx, pr.TenantID, taxNumber)
		if err != nil {
			return nil, fmt.Errorf("partner: fingerprint of a tax registration number: %w", err)
		}
		print = &f
	}
	rows, _ := tx.Query(ctx, `SELECT p.id, p.number, p.name FROM partner.business_partner p
		WHERE p.status <> 'pending' AND (($1 <> '' AND p.number = $1)
			OR p.id IN (SELECT t.partner_id FROM partner.tax_identifier t WHERE $2::text IS NOT NULL AND t.value_fingerprint = $2))
		ORDER BY p.number, p.id`, number, print)
	found, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Found])
	if err != nil {
		return nil, fmt.Errorf("partner: customers named %q, %q: %w", number, taxNumber, err)
	}
	return found, nil
}

// HasTaxNumber says whether the business partner holds a tax identifier
// of the value, whatever its scheme, found through the identifiers' keyed
// hash without opening one (ADR-0061 §6: an e-invoice's seller TRN held
// against the order's supplier). An empty value is held by none.
func (p Payees) HasTaxNumber(ctx context.Context, partnerID uuid.UUID, value string) (bool, error) {
	tx, pr, err := reqctx.Request(ctx)
	if err != nil {
		return false, err
	}
	if value == "" {
		return false, nil
	}
	print, err := p.Keys.Fingerprint(ctx, pr.TenantID, value)
	if err != nil {
		return false, fmt.Errorf("partner: fingerprint of a tax registration number: %w", err)
	}
	var held bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM partner.tax_identifier WHERE partner_id = $1 AND value_fingerprint = $2)`,
		partnerID, print).Scan(&held); err != nil {
		return false, fmt.Errorf("partner: tax identifiers of partner %s: %w", partnerID, err)
	}
	return held, nil
}
