package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/db"
	"github.com/mustie7iy/agentic-erp/internal/analytics/extract"
	"github.com/mustie7iy/agentic-erp/internal/controlplane"
	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/drverify"
	"github.com/mustie7iy/agentic-erp/internal/offboarding"
	platformconfig "github.com/mustie7iy/agentic-erp/internal/platform/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
)

// reconcileCmd brings a restored cell in line with the object store and
// the directory before it serves (ADR-0040, T41f):
//
//	erp-server dr reconcile
//
// It runs with the cell's own configuration (config.Load: the object store,
// the directory, ERP_CELL), after erp-server dr verify passed and before
// the workloads start: the chains against their anchors, the analytical
// stores ahead of the database rebuilt, every tenant the directory places
// in the cell present, the tenants the directory has offboarded or erased
// offboarded and erased again. The report is JSON; a
// problem exits 1.
func reconcileCmd(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "erp-server dr reconcile takes no arguments")
		return 2
	}
	cfg, err := platformconfig.Load(getenv)
	if err != nil {
		fmt.Fprintln(stderr, "erp-server dr reconcile: configuration:", err)
		return 2
	}
	rep, err := reconcile(ctx, cfg, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "erp-server dr reconcile:", err)
		return 1
	}
	out, _ := json.MarshalIndent(rep, "", "  ") // ids, counts and strings always encode
	fmt.Fprintln(stdout, string(out))
	if !rep.Passed() {
		fmt.Fprintf(stderr, "erp-server dr reconcile: %d problems\n", len(rep.Problems))
		return 1
	}
	return 0
}

func reconcile(ctx context.Context, cfg platformconfig.Config, stderr io.Writer) (drverify.Reconciled, error) {
	store := objects(cfg)
	if store == nil {
		return drverify.Reconciled{}, errors.New("no object storage configured (ERP_OBJECT_DIR or ERP_OBJECT_S3_*): the anchors and exports are there")
	}
	pool, err := database.Open(ctx, cfg.DatabaseURL, drSettings)
	if err != nil {
		return drverify.Reconciled{}, fmt.Errorf("the restored database: %w", err)
	}
	defer pool.Close()
	log := telemetry.NewLogger(stderr, cfg.LogLevel)
	rc := &drverify.Reconciler{
		Verifier: &drverify.Verifier{Pool: pool, AppRole: cfg.AppRole, ControlRole: cfg.ControlRole, Want: db.LatestVersion()},
		Store:    store, Cell: cfg.Cell,
		// No directory on the control plane: the directory is the record
		// here, and the cell's sync writes it once the cell runs.
		Control: &controlplane.Service{Pool: pool, AppRole: cfg.AppRole, ControlRole: cfg.ControlRole},
		Offboarding: &offboarding.Worker{Pool: pool, Store: store, AppRole: cfg.AppRole, WorkerRole: cfg.WorkerRole,
			Grace: cfg.OffboardGrace, Retention: cfg.Objects.Retention, Log: log},
		// The analytical stores the restore left ahead of the database
		// are rebuilt (ADR-0044): their watermarks name transactions the
		// restored database has not reached.
		Analytics: func(ctx context.Context) ([]uuid.UUID, error) {
			return extract.RebuildAhead(ctx, pool.Begin, cfg.WorkerRole, store)
		},
	}
	if cfg.DirectoryURL != "" {
		dir, err := directory.OpenWith(ctx, cfg.DirectoryURL, cfg.DirectoryPassword)
		if err != nil {
			return drverify.Reconciled{}, err
		}
		defer dir.Close()
		rc.Directory = dir
	}
	return rc.Reconcile(ctx)
}
