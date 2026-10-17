package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/agents"
	platformconfig "github.com/mustie7iy/agentic-erp/internal/platform/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
)

// pricesCmd records models' published prices, which the model gateway
// needs before it calls a model (ADR-0036):
//
//	erp-server prices load -file prices.json -by <operator>
//
// The file is a JSON list of {"model", "effective_from", "currency",
// "input", "output", "cache_write", "cache_read"}, amounts per million
// tokens in the currency's major units. The prices and who recorded them
// stand as recorded; a change is a new price from a later date.
func pricesCmd(ctx context.Context, args []string, cfg platformconfig.Config, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "load" {
		fmt.Fprintln(stderr, "erp-server prices: the command is erp-server prices load -file prices.json -by <operator>")
		return 2
	}
	fs := flag.NewFlagSet("erp-server prices load", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("file", "", "a JSON list of prices per million tokens")
	by := fs.String("by", "", "the operator recording them, kept with each price")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *file == "" || *by == "" {
		fmt.Fprintln(stderr, "erp-server prices load: -file and -by are required")
		return 2
	}
	n, err := loadPrices(ctx, cfg, *file, *by)
	if err != nil {
		fmt.Fprintln(stderr, "erp-server prices load:", err)
		return 1
	}
	fmt.Fprintf(stdout, "%d prices recorded by %s\n", n, *by)
	return 0
}

// loadPrices records the file's prices in one transaction: all or none.
func loadPrices(ctx context.Context, cfg platformconfig.Config, file, by string) (int, error) {
	raw, err := os.ReadFile(file) // #nosec G304 -- the operator names the file to load
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", file, err)
	}
	var prices []agents.Price
	if err := json.Unmarshal(raw, &prices); err != nil {
		return 0, fmt.Errorf("%s is not a JSON list of prices: %w", file, err)
	}
	pool, err := database.Open(ctx, cfg.DatabaseURL, database.DefaultSettings)
	if err != nil {
		return 0, err
	}
	defer pool.Close()
	// As the login role, which owns the reference data: the application's
	// role reads prices and never writes them.
	if err := pool.InTx(ctx, database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
		return agents.RecordPrices(ctx, tx, by, prices)
	}); err != nil {
		return 0, err
	}
	return len(prices), nil
}
