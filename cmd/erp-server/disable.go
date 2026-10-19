package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/controlplane"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	platformconfig "github.com/mustie7iy/agentic-erp/internal/platform/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// disableConnectorCmd disables a tenant's connector without the server, as
// POST /v1/connectors/{id}:disable does it (locked, audited, published):
//
//	erp-server disable-connector -tenant <id> -connector <id> -by <operator> -reason <text>
//
// It is the remedy a production cell names when it refuses to start for an
// active bank_ebics connector (ADR-0038): the API is served by the cell it
// stopped. The audit entry names the operator and the reason; the event
// waits in the outbox until the cell runs again.
func disableConnectorCmd(ctx context.Context, args []string, cfg platformconfig.Config, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("erp-server disable-connector", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tenant := fs.String("tenant", "", "the tenant's id")
	connector := fs.String("connector", "", "the connector's id")
	by := fs.String("by", "", "the operator disabling it, by a handle rather than an email address (the audit trail refuses personal data, ADR-0026)")
	reason := fs.String("reason", "", "why, kept in the audit entry")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	t, terr := uuid.Parse(*tenant)
	c, cerr := uuid.Parse(*connector)
	if terr != nil || cerr != nil || *by == "" || *reason == "" {
		fmt.Fprintln(stderr, "erp-server disable-connector: -tenant and -connector must be ids, -by and -reason are required")
		return 2
	}
	if err := disableConnector(ctx, cfg, t, c, *by, *reason); err != nil {
		fmt.Fprintln(stderr, "erp-server disable-connector:", err)
		return 1
	}
	fmt.Fprintf(stdout, "connector %s of tenant %s disabled by %s\n", c, t, *by)
	return 0
}

// disableConnector disables the connector in the tenant's scope, as the
// operator named for the command.
func disableConnector(ctx context.Context, cfg platformconfig.Config, tenant, connector uuid.UUID, by, reason string) error {
	pool, err := database.Open(ctx, cfg.DatabaseURL, database.DefaultSettings)
	if err != nil {
		return err
	}
	defer pool.Close()
	op := controlplane.OperatorFrom(reqctx.Claims{Issuer: "erp-server", Subject: by})
	actor := reqctx.Principal{ID: op.ID, Kind: reqctx.KindSystem, TenantID: tenant}
	job := uow.Job{Begin: pool.Begin, Scope: database.Scope{Role: cfg.AppRole, TenantID: tenant}, Name: "disable connector " + connector.String()}
	return job.Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return true, uow.Act(ctx, tx, actor, nil, func(ctx context.Context) error {
			_, err := integration.DisableByOperator(ctx, connector, by, reason)
			return err
		})
	})
}
