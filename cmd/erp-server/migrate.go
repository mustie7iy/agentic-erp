package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"

	platformconfig "github.com/mustie7iy/agentic-erp/internal/platform/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
)

// migrateCmd applies the pending migrations and creates the partitions the
// coming periods need, then exits:
//
//	erp-server migrate
//
// It is what the Helm chart's pre-install and pre-upgrade hook Job runs,
// once per release and cell (ADR-0035), while the server's pods keep
// ERP_MIGRATE_ON_START=false and become ready only on the schema they were
// built for. It reads only what preparing the schema needs
// (config.LoadSchema), so the Job carries no key, issuer or store and runs
// before the release's other resources exist. Its DATABASE_URL is the
// database's writer itself, never PgBouncer: the runner holds a session
// advisory lock and runs statements outside transactions, which
// transaction pooling breaks. Runs started together serialize on the
// runner's lock, and a later run applies nothing; what a run applied is
// logged. A run interrupted by SIGTERM exits 1 and the Job retries it: a
// migration in a transaction was rolled back and runs again; one built
// CONCURRENTLY outside a transaction may leave an invalid index that the
// retry refuses, dropped by hand as that migration says.
func migrateCmd(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer) int {
	cfg, err := platformconfig.LoadSchema(getenv)
	if err != nil {
		fmt.Fprintln(stderr, "erp-server migrate: configuration:", err)
		return 2
	}
	log := telemetry.NewLogger(stderr, cfg.LogLevel)
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
