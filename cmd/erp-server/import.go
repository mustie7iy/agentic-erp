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

	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/offboarding"
	platformconfig "github.com/mustie7iy/agentic-erp/internal/platform/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
)

// importCmd brings an exported tenant into this cell (ADR-0029):
//
//	erp-server import -manifest tenants/<id>/exports/<time>/manifest.json
//
// The manifest and its parts are read from the cell's object store; the
// tenant row, every table's rows and the audit chain come back as exported,
// verified against the manifest's counts, hashes and chain head, in one
// transaction. The tenant must be absent from the cell or erased here. It
// prints what was imported as JSON.
func importCmd(ctx context.Context, args []string, cfg platformconfig.Config, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("erp-server import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	manifest := fs.String("manifest", "", "object key of the export's manifest")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	res, err := importTenant(ctx, cfg, *manifest)
	if err != nil {
		fmt.Fprintln(stderr, "erp-server import:", err)
		return 1
	}
	out, _ := json.MarshalIndent(res, "", "  ") // counts and ids always encode
	fmt.Fprintln(stdout, string(out))
	return 0
}

func importTenant(ctx context.Context, cfg platformconfig.Config, manifest string) (offboarding.Imported, error) {
	if manifest == "" {
		return offboarding.Imported{}, errors.New("-manifest is required: the object key of the export's manifest")
	}
	store := objects(cfg)
	if store == nil {
		return offboarding.Imported{}, errors.New("no object storage configured (ERP_OBJECT_DIR or ERP_OBJECT_S3_*) to read the export from")
	}
	pool, err := database.Open(ctx, cfg.DatabaseURL, database.DefaultSettings)
	if err != nil {
		return offboarding.Imported{}, err
	}
	defer pool.Close()
	im := &offboarding.Importer{Pool: pool, Store: store, AppRole: cfg.AppRole}
	var checks []func(context.Context, pgx.Tx, uuid.UUID) error
	if cfg.Environment == platformconfig.Production {
		checks = append(checks, productionImport) // a production cell refuses to start with such a tenant
	}
	if cfg.DirectoryURL != "" {
		dir, err := directory.OpenWith(ctx, cfg.DirectoryURL, cfg.DirectoryPassword)
		if err != nil {
			return offboarding.Imported{}, err
		}
		defer dir.Close()
		// The directory keeps an offboarded or erased tenant so against the
		// cell's sync (ADR-0040); the import brings it back. Before the
		// commit: should the import then fail, the cell's next sync writes
		// the erased tenant back, which an active entry does not keep.
		checks = append(checks, func(ctx context.Context, _ pgx.Tx, tenant uuid.UUID) error {
			return dir.Reinstate(ctx, tenant, cfg.Cell)
		})
	}
	im.Check = func(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) error {
		for _, check := range checks {
			if err := check(ctx, tx, tenant); err != nil {
				return err
			}
		}
		return nil
	}
	return im.Import(ctx, manifest)
}

// productionImport refuses, in production, a tenant without an active
// identity provider (ADR-0031) or with an active bank_ebics connector
// (ADR-0038, T44d): a production cell refuses to start with either.
func productionImport(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) error {
	if err := identity.RequireActiveProvider(ctx, tx, tenant); err != nil {
		return err
	}
	return integration.RefuseActiveEBICS(ctx, tx, tenant)
}
