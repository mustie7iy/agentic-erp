package payments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// SentFiles records the content hash of each file a house bank accepted
// (payments.sent_bank_file; ADR-0038, T44c) in a transaction of its own,
// committed right after the upload: the request's transaction marks the
// file transmitted only later, and if its commit is lost the record stays,
// so the retry finds the hash and sends nothing twice.
type SentFiles struct {
	Pool    *database.Pool
	AppRole string             // erp_app: the record is written in the tenant's scope
	Begin   database.BeginFunc // nil: Pool.Begin; tests inject failures
	Timeout time.Duration      // how long the record may take, a second connection included; default 10s
}

func (s SentFiles) begin(ctx context.Context, sc database.Scope) (pgx.Tx, error) {
	if s.Pool == nil && s.Begin == nil {
		return nil, errors.New("no database to record sent bank files on")
	}
	return database.BeginOr(ctx, s.Pool, s.Begin, sc)
}

// record commits that the file's bank took it. The request's cancellation
// does not stop it, since the bank already has the file, but a bound does:
// the record needs a second connection while the request holds one, and
// with every connection held by transmissions waiting for it the pool
// would never hand one out. Past the bound the transmission fails and the
// file stays created (ADR-0038). This row is the audit record of the upload
// (ADR-0011): the request's audit entry follows only if it commits.
func (s SentFiles) record(ctx context.Context, p reqctx.Principal, f BankFile, connector *uuid.UUID) error {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	job := uow.Job{Begin: s.begin, Scope: database.Scope{Role: s.AppRole, TenantID: p.TenantID}, Name: "payments: record sent bank file " + f.ID.String()}
	err := job.Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		_, err := tx.Exec(ctx, `INSERT INTO payments.sent_bank_file (tenant_id, house_bank_id, sha256, bank_file_id, connector_id, sent_by)
			VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`, p.TenantID, f.HouseBankID, f.SHA256, f.ID, connector, p.ID)
		if err != nil {
			return false, fmt.Errorf("insert: %w", err)
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("payments: record sent bank file %s: %w", f.ID, err)
	}
	return nil
}

// sent reports whether the file's bank already took this file, by its
// content hash: a transmission whose own commit was lost (T44c). The file
// must be the one recorded too, so that should two files ever share
// content (each file's message id makes it unique today), the second is
// not marked transmitted without being uploaded.
func sent(ctx context.Context, tx pgx.Tx, f BankFile) (bool, error) {
	var found bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM payments.sent_bank_file WHERE house_bank_id = $1 AND sha256 = $2 AND bank_file_id = $3)`,
		f.HouseBankID, f.SHA256, f.ID).Scan(&found); err != nil {
		return false, fmt.Errorf("payments: was bank file %s sent: %w", f.ID, err)
	}
	return found, nil
}
