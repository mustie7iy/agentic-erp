package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
)

type migrationRun struct {
	ID     string
	Status string
	Report struct {
		Quality, Load []struct{ Severity, Table, Key, Message string }
		Loaded        map[string]int
		Checks        []struct {
			CompanyCode string `json:"company_code"`
			Name        string
			SAP         string
			Platform    string
			Pass        bool
		}
		Diffs []struct {
			CompanyCode string `json:"company_code"`
			Key         string
			SAP         string
			Platform    string
		}
		Missing []string `json:"missing_documents"`
	}
}

// act posts a custom method of a resource with its current ETag and, when
// the answer is an operation still running (a cell with a workflow engine,
// ADR-0012), waits for it to finish.
func act(ctx context.Context, c *apitools.Client, path, method string) error {
	got, err := c.Do(ctx, http.MethodGet, path, nil, nil, "")
	if err != nil {
		return err
	}
	if err := got.Err(); err != nil {
		return err
	}
	resp, err := c.Do(ctx, http.MethodPost, path+":"+method, nil, nil, got.ETag)
	if err != nil {
		return err
	}
	if err := resp.Err(); err != nil {
		return err
	}
	op, ok := operationOf(resp)
	if !ok {
		return nil // not an operation: nothing to wait for
	}
	for op.Status == "queued" || op.Status == "running" {
		time.Sleep(pollInterval)
		resp, err := c.Do(ctx, http.MethodGet, "/v1/operations/"+op.ID, nil, nil, "")
		if err != nil {
			return err
		}
		if err := resp.Err(); err != nil {
			return err
		}
		if err := resp.Decode(&op); err != nil {
			return err
		}
	}
	return nil
}

// operation is the part of an Operation resource act waits on.
type operation struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// operationOf reads an answer as an operation; an answer that is not one
// (the action completed within the request) is not an error.
func operationOf(resp apitools.Response) (operation, bool) {
	var op operation
	return op, resp.Decode(&op) == nil && op.ID != ""
}

// pollInterval is how often a running operation is read; tests shorten it.
var pollInterval = 500 * time.Millisecond

// migrate runs a migration scope through the integration API: define the
// run, start it, reconcile it when it loaded, and print the report. It
// fails unless the run reconciled, the condition for cutover
// (docs/migration.md).
func migrate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	connector := fs.String("connector", "", "source connector id")
	entities := fs.String("legal-entities", "", "comma-separated legal entity ids")
	year := fs.Int("year", 2026, "fiscal year")
	mode := fs.String("mode", "documents", "documents or balances")
	rehearsal := fs.Bool("rehearsal", false, "load, reconcile and roll back, keeping only the report")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	c, err := client("ERP_TOKEN")
	if err != nil {
		return err
	}
	resp, err := c.Do(ctx, http.MethodPost, "/v1/migration-runs", nil, map[string]any{"connector_id": *connector, "mode": *mode,
		"is_rehearsal": *rehearsal, "scope": map[string]any{"legal_entity_ids": strings.Split(*entities, ","), "fiscal_year": *year}}, "")
	if err != nil {
		return err
	}
	var run migrationRun
	if err := resp.Decode(&run); err != nil {
		return err
	}
	path := "/v1/migration-runs/" + run.ID
	if err := act(ctx, c, path, "start"); err != nil {
		return err
	}
	if run, err = readRun(ctx, c, path); err != nil {
		return err
	}
	if run.Status == "loaded" {
		if err := act(ctx, c, path, "reconcile"); err != nil {
			return err
		}
		if run, err = readRun(ctx, c, path); err != nil {
			return err
		}
	}
	printed := printRun(run)
	if run.Status != "reconciled" {
		return errors.Join(fmt.Errorf("migration run %s is %s: do not cut over this scope", run.ID, run.Status), printed)
	}
	return printed
}

func readRun(ctx context.Context, c *apitools.Client, path string) (migrationRun, error) {
	var run migrationRun
	resp, err := c.Do(ctx, http.MethodGet, path, nil, nil, "")
	if err == nil {
		err = resp.Decode(&run)
	}
	return run, err
}

// printRun prints the run's report. Its error is a checks table that could
// not be written, so the operator never decides on a partial report.
func printRun(run migrationRun) error {
	r := run.Report
	loaded, _ := json.Marshal(r.Loaded)
	fmt.Fprintf(out, "Migration run %s: %s\nLoaded: %s\n", run.ID, run.Status, loaded)
	for _, list := range []struct {
		title  string
		issues []struct{ Severity, Table, Key, Message string }
	}{{"Quality findings", r.Quality}, {"Load refusals", r.Load}} {
		if len(list.issues) > 0 {
			fmt.Fprintf(out, "\n%s:\n", list.title)
			for _, i := range list.issues {
				fmt.Fprintf(out, "  [%s] %s %s: %s\n", i.Severity, i.Table, i.Key, i.Message)
			}
		}
	}
	if len(r.Checks) > 0 {
		fmt.Fprintln(out)
		tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "COMPANY\tCHECK\tSAP\tPLATFORM\tRESULT")
		for _, c := range r.Checks {
			res := "PASS"
			if !c.Pass {
				res = "FAIL"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", c.CompanyCode, c.Name, c.SAP, c.Platform, res)
		}
		if err := tw.Flush(); err != nil {
			return fmt.Errorf("migrate: print checks: %w", err)
		}
	}
	for _, d := range r.Diffs {
		fmt.Fprintf(out, "  difference %s %s: SAP %s, platform %s\n", d.CompanyCode, d.Key, d.SAP, d.Platform)
	}
	if len(r.Missing) > 0 {
		fmt.Fprintf(out, "  %d SAP documents are not on the platform, for example %s\n", len(r.Missing), r.Missing[0])
	}
	return nil
}
