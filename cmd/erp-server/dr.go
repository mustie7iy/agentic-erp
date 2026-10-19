package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/db"
	"github.com/mustie7iy/agentic-erp/internal/drverify"
	platformconfig "github.com/mustie7iy/agentic-erp/internal/platform/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
)

// drSettings let the checks read whole tables of a restored database,
// which nothing else uses yet: no statement timeout.
var drSettings = database.Settings{LockTimeout: 5 * time.Second}

// drCmd checks a restored cell and writes a restore drill's markers
// (ADR-0040, T41d):
//
//	erp-server dr mark -tenant ID -name NAME
//	erp-server dr verify [-restored FILE] [-lost FILE]
//
// mark writes a marker into the tenant's audit chain and prints it, with
// the tenant's fingerprint, as JSON. verify runs every check against
// DATABASE_URL and prints the report as JSON; it exits 1 when a check
// failed. -restored is the marker written before the restore point (it
// must be there, the tenant's fingerprint as it was), -lost the one
// written after (it must not). ERP_DR_OLD_DATABASE_URL, when set, is the
// database the cell ran on before: every tenant's fingerprint is compared
// with its own there. Like migrate, it reads only the database settings
// (config.LoadSchema): a drill runs it as a one-off pod against a side
// database, with no keys, issuers or stores.
func drCmd(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "mark" && args[0] != "verify") {
		fmt.Fprintln(stderr, "erp-server dr: mark or verify (erp-server dr mark -tenant ID -name NAME; erp-server dr verify [-restored FILE] [-lost FILE])")
		return 2
	}
	fs := flag.NewFlagSet("erp-server dr "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	tenant := fs.String("tenant", "", "mark: the drill tenant's id")
	name := fs.String("name", "", "mark: the marker's name")
	restored := fs.String("restored", "", "verify: the marker written before the restore point, as mark printed it")
	lost := fs.String("lost", "", "verify: the marker written after the restore point, as mark printed it")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	cfg, err := platformconfig.LoadSchema(getenv)
	if err != nil {
		fmt.Fprintln(stderr, "erp-server dr: configuration:", err)
		return 2
	}
	run := func(v *drverify.Verifier) (any, error) { return drVerify(ctx, v, *restored, *lost, getenv) }
	if args[0] == "mark" {
		run = func(v *drverify.Verifier) (any, error) { return drMark(ctx, v, *tenant, *name) }
	}
	res, err := withVerifier(ctx, cfg, run)
	if err != nil {
		fmt.Fprintf(stderr, "erp-server dr %s: %v\n", args[0], err)
		return 1
	}
	out, _ := json.MarshalIndent(res, "", "  ") // ids, counts and strings always encode
	fmt.Fprintln(stdout, string(out))
	if r, ok := res.(drverify.Report); ok && !r.Passed() {
		fmt.Fprintf(stderr, "erp-server dr verify: %d checks failed\n", len(r.Problems))
		return 1
	}
	return 0
}

// withVerifier opens the restored database and runs fn with a verifier on it.
func withVerifier(ctx context.Context, cfg platformconfig.Config, fn func(*drverify.Verifier) (any, error)) (any, error) {
	pool, err := database.Open(ctx, cfg.DatabaseURL, drSettings)
	if err != nil {
		return nil, fmt.Errorf("the restored database: %w", err)
	}
	defer pool.Close()
	return fn(&drverify.Verifier{Pool: pool, AppRole: cfg.AppRole, ControlRole: cfg.ControlRole, Want: db.LatestVersion()})
}

func drMark(ctx context.Context, v *drverify.Verifier, tenant, name string) (drverify.Mark, error) {
	id, err := uuid.Parse(tenant)
	if err != nil {
		return drverify.Mark{}, fmt.Errorf("-tenant: %w", err)
	}
	return v.Mark(ctx, id, name)
}

func drVerify(ctx context.Context, v *drverify.Verifier, restored, lost string, getenv func(string) string) (drverify.Report, error) {
	var err error
	if v.Restored, err = readMark(restored); err != nil {
		return drverify.Report{}, err
	}
	if v.Lost, err = readMark(lost); err != nil {
		return drverify.Report{}, err
	}
	if url := getenv("ERP_DR_OLD_DATABASE_URL"); url != "" {
		if v.Old, err = database.Open(ctx, url, drSettings); err != nil {
			return drverify.Report{}, fmt.Errorf("the old database: %w", err)
		}
		defer v.Old.Close()
	}
	return v.Verify(ctx)
}

// readMark reads a marker as mark printed it; nil without a file.
func readMark(path string) (*drverify.Mark, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- the operator names the file
	if err != nil {
		return nil, fmt.Errorf("marker: %w", err)
	}
	var m drverify.Mark
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("marker %s: %w", path, err)
	}
	return &m, nil
}
