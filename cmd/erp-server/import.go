package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/mustie7iy/agentic-erp/internal/identity"
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
	store := objects(cfg.Objects, cfg.AWS)
	if store == nil {
		return offboarding.Imported{}, errors.New("no object storage configured (ERP_OBJECT_DIR or ERP_OBJECT_S3_*) to read the export from")
	}
	pool, err := database.Open(ctx, cfg.DatabaseURL, database.DefaultSettings)
	if err != nil {
		return offboarding.Imported{}, err
	}
	defer pool.Close()
	im := &offboarding.Importer{Pool: pool, Store: store, AppRole: cfg.AppRole}
	if cfg.Environment == platformconfig.Production {
		im.Check = identity.RequireActiveProvider // a production cell refuses to start with such a tenant
	}
	return im.Import(ctx, manifest)
}
