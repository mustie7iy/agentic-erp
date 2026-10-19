package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/agents"
	"github.com/mustie7iy/agentic-erp/internal/eval"
	platformconfig "github.com/mustie7iy/agentic-erp/internal/platform/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
)

// certifyCmd loads an evaluation run's report as its agent type's
// certification on the model that answered it (ADR-0039):
//
//	erp-server certify load -report <file> -by <operator>
//
// Only a report that meets a level is loaded: a complete run of three
// repeats per case, on one model, with no safety failure and the level's
// pass rate, regraded here from its repeats. The certification, the
// report's SHA-256 and who loaded it stand as loaded: the rows are the
// trail, and a later run is a new row.
func certifyCmd(ctx context.Context, args []string, cfg platformconfig.Config, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "load" {
		fmt.Fprintln(stderr, "erp-server certify: the command is erp-server certify load -report <file> -by <operator>")
		return 2
	}
	fs := flag.NewFlagSet("erp-server certify load", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("report", "", "an evaluation run's report (evals/results/<agent>/<model>/…json)")
	by := fs.String("by", "", "the operator loading it, kept with the certification")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *file == "" || *by == "" {
		fmt.Fprintln(stderr, "erp-server certify load: -report and -by are required")
		return 2
	}
	c, err := loadCertification(ctx, cfg, *file, *by)
	if err != nil {
		fmt.Fprintln(stderr, "erp-server certify load:", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s certified for level %d on %s by %s (suite version %d, report %s)\n", c.AgentType, c.Level, c.Model, *by, c.SuiteVersion, c.ReportSHA256)
	return 0
}

// loadCertification reads the report, checks what it certifies and
// records it.
func loadCertification(ctx context.Context, cfg platformconfig.Config, file, by string) (agents.Certification, error) {
	raw, err := os.ReadFile(file) // #nosec G304 -- the operator names the report to load
	if err != nil {
		return agents.Certification{}, fmt.Errorf("read %s: %w", file, err)
	}
	var r eval.Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return agents.Certification{}, fmt.Errorf("%s is not an evaluation report: %w", file, err)
	}
	level, model, err := r.Certifies()
	if err != nil {
		return agents.Certification{}, err
	}
	sum := sha256.Sum256(raw)
	c := agents.Certification{AgentType: r.Agent, Model: model, Level: int16(level), SuiteVersion: r.SuiteVersion, ReportSHA256: hex.EncodeToString(sum[:])} // #nosec G115 -- a level is 1 or 2
	pool, err := database.Open(ctx, cfg.DatabaseURL, database.DefaultSettings)
	if err != nil {
		return c, err
	}
	defer pool.Close()
	// As the login role, which owns the reference data: the application's
	// role reads certifications and never writes them.
	return c, pool.InTx(ctx, database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
		return agents.RecordCertification(ctx, tx, by, c)
	})
}
