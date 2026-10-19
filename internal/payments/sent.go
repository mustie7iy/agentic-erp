package payments

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// recordSent records that the file's bank took it (payments.sent_bank_file;
// ADR-0038, T44c), in the transaction that marks it transmitted (T44e):
// the upload ran with no transaction open, so the record and the status
// commit together or not at all. If that commit is lost, the claim lapses,
// the file is sent again and the bank refuses the repeated message id. The
// row is the upload's audit record (ADR-0011), naming who asked for it.
func recordSent(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, f BankFile, connector *uuid.UUID, by uuid.UUID) error {
	if _, err := tx.Exec(ctx, `INSERT INTO payments.sent_bank_file (tenant_id, house_bank_id, sha256, bank_file_id, connector_id, sent_by)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`, tenant, f.HouseBankID, f.SHA256, f.ID, connector, by); err != nil {
		return fmt.Errorf("payments: record sent bank file %s: %w", f.ID, err)
	}
	return nil
}

// sent reports whether the file's bank already took this file, by its
// content hash (T44c). The file must be the one recorded too, so that
// should two files ever share content (each file's message id makes it
// unique today), the second is not marked transmitted without being
// uploaded.
func sent(ctx context.Context, tx pgx.Tx, f BankFile) (bool, error) {
	var found bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM payments.sent_bank_file WHERE house_bank_id = $1 AND sha256 = $2 AND bank_file_id = $3)`,
		f.HouseBankID, f.SHA256, f.ID).Scan(&found); err != nil {
		return false, fmt.Errorf("payments: was bank file %s sent: %w", f.ID, err)
	}
	return found, nil
}
