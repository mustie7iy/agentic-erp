package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	platformconfig "github.com/mustie7iy/agentic-erp/internal/platform/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
)

// rewrapCmd moves every tenant's field-encryption keys onto the ring's
// first master, the one new keys are wrapped with (the KMS key when
// ERP_FIELD_KMS_KEY is set, else the first of ERP_FIELD_KEYS):
//
//	erp-server rewrap
//
// Run it after putting a new master in front of the ring and before taking
// the masters it replaces out of it; the tenant keys do not change, so no
// sealed value is rewritten. It prints what it did as JSON. Tenants are
// listed with the control role and each tenant's keys are re-wrapped in a
// transaction of its own scope; the first tenant that fails stops the run,
// and running again continues where it stopped.
func rewrapCmd(ctx context.Context, args []string, cfg platformconfig.Config, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("erp-server rewrap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	res, err := rewrap(ctx, cfg)
	if err != nil {
		fmt.Fprintln(stderr, "erp-server rewrap:", err)
		return 1
	}
	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Fprintln(stdout, string(out))
	return 0
}

// rewrapResult is what a run did.
type rewrapResult struct {
	Master  string `json:"master_key_id"`
	Tenants int    `json:"tenants"` // tenants visited
	Keys    int    `json:"keys"`    // tenant keys moved onto the master
}

func rewrap(ctx context.Context, cfg platformconfig.Config) (rewrapResult, error) {
	pool, err := database.Open(ctx, cfg.DatabaseURL, database.DefaultSettings)
	if err != nil {
		return rewrapResult{}, err
	}
	defer pool.Close()
	keys, _ := keyring(cfg, pool) // the keys were validated with the configuration, which requires a master
	res := rewrapResult{Master: keys.Masters[0].ID()}
	var tenants []uuid.UUID
	err = pool.InTx(ctx, database.Scope{Role: cfg.ControlRole, ReadOnly: true}, func(ctx context.Context, tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT id FROM tenancy.tenant ORDER BY id`)
		var id uuid.UUID
		_, err := pgx.ForEachRow(rows, []any{&id}, func() error {
			tenants = append(tenants, id)
			return nil
		})
		return err
	})
	if err != nil {
		return rewrapResult{}, fmt.Errorf("listing tenants: %w", err)
	}
	for _, tenant := range tenants {
		var done fieldcrypt.Rewrapped
		err := pool.InTx(ctx, database.Scope{Role: cfg.AppRole, TenantID: tenant}, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			done, err = keys.Rewrap(ctx, tx, tenant)
			return err
		})
		if err != nil {
			return rewrapResult{}, fmt.Errorf("tenant %s: %w", tenant, err)
		}
		res.Tenants++
		res.Keys += done.Keys
	}
	return res, nil
}
