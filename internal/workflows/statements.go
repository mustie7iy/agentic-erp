package workflows

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/sftpclient"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// FetchInput names the house bank whose statements a fetch workflow fetches.
type FetchInput struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	HouseBankID uuid.UUID `json:"house_bank_id"`
}

// FileInput names a file in the bank's statements directory; SHA256 is
// its content's, once imported.
type FileInput struct {
	FetchInput
	Name   string `json:"name"`
	SHA256 []byte `json:"sha256,omitempty"`
}

// FetchWorkflow is the registered name of FetchBankStatements.
const FetchWorkflow = "FetchBankStatements"

// FetchWorkflowID is the id of a house bank's fetch in the period starting
// at slot: one fetch per bank and period however many dispatchers ask
// (ADR-0047, the engine refuses a known id).
func FetchWorkflowID(tenant, houseBank uuid.UUID, slot time.Time) string {
	return fmt.Sprintf("%s-house_bank-%s-statements-%d", tenant, houseBank, slot.Unix())
}

// FetchBankStatements fetches the house bank's statements from its SFTP
// channel (T49d): it lists the statements directory, then per file
// imports it and, once imported, moves it to the archive directory. A
// file's failure, after the activity's retries, does not stop the others:
// the workflow fails at the end naming the files left, which the next
// fetch takes again.
func FetchBankStatements(ctx workflow.Context, in FetchInput) error {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: time.Minute, MaximumAttempts: 5}})
	var a *Statements
	var names []string
	if err := workflow.ExecuteActivity(ctx, a.ListStatementFiles, in).Get(ctx, &names); err != nil {
		return err //nolint:wrapcheck // workflow result: the engine records Future.Get's ActivityError, which carries the activity type and id, as the failure; a wrap would put a generic wrapError failure on top and add no context
	}
	var failed []string
	for _, name := range names {
		var f payments.Fetched
		if err := workflow.ExecuteActivity(ctx, a.ImportStatementFile, FileInput{FetchInput: in, Name: name}).Get(ctx, &f); err != nil {
			failed = append(failed, name)
			continue
		}
		if f.Status != payments.FetchImported && f.Status != payments.FetchArchived {
			continue // refused, or another bank's: left in the directory
		}
		if err := workflow.ExecuteActivity(ctx, a.ArchiveStatementFile, FileInput{FetchInput: in, Name: name, SHA256: f.SHA256}).Get(ctx, nil); err != nil {
			failed = append(failed, name)
		}
	}
	if len(failed) > 0 {
		return temporal.NewNonRetryableApplicationError("files not fetched: "+strings.Join(failed, ", "), "StatementFilesFailed", nil) //nolint:wrapcheck // constructs the engine's non-retryable ApplicationError; nothing to wrap
	}
	return nil
}

// Statements holds the statement fetch activities. Each resolves the
// bank's channel in a transaction of its own, talks to the server with no
// transaction open, and records what it did in another, as the fetcher.
type Statements struct {
	Pool      *database.Pool
	AppRole   string // erp_app: in the tenant's scope
	Endpoints *payments.Endpoints
	Now       func() time.Time
	// Begin starts transactions; nil means Pool.Begin. Tests inject failures.
	Begin database.BeginFunc
}

func (s *Statements) begin(ctx context.Context, sc database.Scope) (pgx.Tx, error) {
	return database.BeginOr(ctx, s.Pool, s.Begin, sc)
}

// as runs fn in one transaction of the tenant as the fetcher.
func (s *Statements) as(ctx context.Context, in FetchInput, fn func(ctx context.Context) error) error {
	return uow.Job{Begin: s.begin, Scope: database.Scope{Role: s.AppRole, TenantID: in.TenantID}, Now: s.Now,
		Name: fmt.Sprintf("workflows: fetch statements of house bank %s", in.HouseBankID)}.As(ctx,
		func(context.Context, pgx.Tx) (reqctx.Principal, bool, error) {
			return payments.Fetcher(in.TenantID), true, nil
		}, fn)
}

// channel resolves the bank's channel; a refusal (no channel, disabled,
// a key that does not resolve) is not retried.
func (s *Statements) channel(ctx context.Context, in FetchInput) (sftpclient.Endpoint, error) {
	var ep sftpclient.Endpoint
	err := s.as(ctx, in, func(ctx context.Context) error {
		var err error
		ep, err = s.Endpoints.StatementChannel(ctx, in.HouseBankID)
		return err
	})
	return ep, asProblem(err)
}

// ListStatementFiles lists the files in the bank's statements directory.
func (s *Statements) ListStatementFiles(ctx context.Context, in FetchInput) ([]string, error) {
	ep, err := s.channel(ctx, in)
	if err != nil {
		return nil, err
	}
	names, err := ep.List(ctx, ep.Inbound)
	if err != nil {
		return nil, fmt.Errorf("workflows: list the statements of house bank %s: %w", in.HouseBankID, err)
	}
	return names, nil
}

// ImportStatementFile reads the file and imports it, once per content. A
// refusal is recorded once and the file reported refused; a statement of
// another house bank's account (a directory several banks share) is
// skipped and left for that bank.
func (s *Statements) ImportStatementFile(ctx context.Context, in FileInput) (payments.Fetched, error) {
	ep, err := s.channel(ctx, in.FetchInput)
	if err != nil {
		return payments.Fetched{}, err
	}
	file, err := ep.Read(ctx, ep.Inbound, in.Name, payments.MaxStatementFile)
	if err != nil {
		return payments.Fetched{}, fmt.Errorf("workflows: read statement file %s of house bank %s: %w", in.Name, in.HouseBankID, err)
	}
	var f payments.Fetched
	err = s.as(ctx, in.FetchInput, func(ctx context.Context) error {
		var err error
		f, err = s.Endpoints.ImportFetched(ctx, in.HouseBankID, in.Name, file)
		return err
	})
	var why *problem.Problem
	if err == nil || !errors.As(err, &why) || why.Status >= 500 {
		return f, err
	}
	if why.Code == "payments.statement_not_of_bank" {
		return payments.Fetched{Status: payments.FetchSkipped}, nil
	}
	sum := sha256.Sum256(file)
	f = payments.Fetched{SHA256: sum[:], Status: payments.FetchRefused}
	return f, s.as(ctx, in.FetchInput, func(ctx context.Context) error {
		return payments.RefuseFetched(ctx, in.HouseBankID, in.Name, f.SHA256, why)
	})
}

// ArchiveStatementFile moves the imported file to the archive directory,
// then records it archived. A move whose acknowledgement was lost finds
// the file moved, and a record already made is left as it is, so running
// it again does nothing twice.
func (s *Statements) ArchiveStatementFile(ctx context.Context, in FileInput) error {
	ep, err := s.channel(ctx, in.FetchInput)
	if err != nil {
		return err
	}
	from, to := payments.StatementPaths(ep, in.Name, in.SHA256)
	if err := ep.Move(ctx, from, to); err != nil {
		return fmt.Errorf("workflows: archive statement file %s of house bank %s: %w", in.Name, in.HouseBankID, err)
	}
	return s.as(ctx, in.FetchInput, func(ctx context.Context) error { return payments.ArchivedFetched(ctx, in.HouseBankID, in.SHA256) })
}
