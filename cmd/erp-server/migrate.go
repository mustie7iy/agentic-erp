package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"

	platformconfig "github.com/mustie7iy/agentic-erp/internal/platform/config"
)

// migrateCmd applies the pending migrations and creates the partitions the
// coming periods need, then exits:
//
//	erp-server migrate
//
// It is what the Helm chart's pre-install and pre-upgrade hook Job runs,
// once per release and cell (ADR-0035), while the server's pods keep
// ERP_MIGRATE_ON_START=false and become ready only on the schema they were
// built for. Runs started together serialize on the runner's lock, and a
// second run applies nothing; what a run applied is logged.
func migrateCmd(ctx context.Context, args []string, cfg platformconfig.Config, log *slog.Logger, stderr io.Writer) int {
	fs := flag.NewFlagSet("erp-server migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "erp-server migrate: takes no arguments, got %q\n", fs.Args())
		return 2
	}
	if err := migrateOnce(ctx, cfg, log); err != nil {
		fmt.Fprintln(stderr, "erp-server migrate:", err)
		return 1
	}
	return 0
}

// migrateOnce prepares the schema as a server starting with
// ERP_MIGRATE_ON_START=true would, and serves nothing.
func migrateOnce(ctx context.Context, cfg platformconfig.Config, log *slog.Logger) error {
	cfg.MigrateOnStart = true
	pool, err := openPool(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	s := &server{cfg: cfg, log: log, pool: pool}
	return s.prepareSchema(ctx)
}
