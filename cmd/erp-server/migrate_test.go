package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/db"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestMigrateCommand: the hook Job's command needs the database and
// nothing else the server does, even in production; it brings an empty
// database to the binary's schema and partitions, runs started together
// serialize on the runner's lock, and a later run applies nothing.
func TestMigrateCommand(t *testing.T) {
	// The Job's whole environment: no issuers, keys or object store.
	env := map[string]string{"DATABASE_URL": pgtest.EmptyDatabase(t), "ERP_ENV": "production"}
	getenv := func(k string) string { return env[k] }
	with := func(k, v string) func(string) string {
		return func(key string) string {
			if key == k {
				return v
			}
			return env[key]
		}
	}

	t.Run("usage", func(t *testing.T) {
		cases := map[string]struct {
			args   []string
			getenv func(string) string
			code   int
			want   string
		}{
			"help":        {[]string{"migrate", "-h"}, getenv, 0, "Usage of erp-server migrate"},
			"bad flag":    {[]string{"migrate", "-x"}, getenv, 2, "flag provided"},
			"argument":    {[]string{"migrate", "now"}, getenv, 2, "takes no arguments"},
			"no database": {[]string{"migrate"}, with("DATABASE_URL", ""), 2, "DATABASE_URL is required"},
			"environment": {[]string{"migrate"}, with("ERP_ENV", "prod"), 2, "ERP_ENV"},
			"log level":   {[]string{"migrate"}, with("ERP_LOG_LEVEL", "loud"), 2, "ERP_LOG_LEVEL"},
			"pool size":   {[]string{"migrate"}, with("ERP_DB_MAX_CONNS", "-1"), 2, "ERP_DB_MAX_CONNS"},
		}
		for name, c := range cases {
			t.Run(name, func(t *testing.T) {
				var stderr bytes.Buffer
				if got := cli(context.Background(), c.args, c.getenv, io.Discard, &stderr, nil); got != c.code {
					t.Errorf("exit code: got %d, want %d (%s)", got, c.code, stderr.String())
				}
				if !strings.Contains(stderr.String(), c.want) {
					t.Errorf("stderr: got %q, want %q in it", stderr.String(), c.want)
				}
			})
		}
	})

	t.Run("database down", func(t *testing.T) {
		var stderr bytes.Buffer
		if got := cli(context.Background(), []string{"migrate"}, with("DATABASE_URL", "postgres://nobody@127.0.0.1:1/x?connect_timeout=1"), io.Discard, &stderr, nil); got != 1 {
			t.Errorf("exit code: got %d, want 1 (%s)", got, stderr.String())
		}
		if !strings.Contains(stderr.String(), "erp-server migrate:") {
			t.Errorf("stderr: got %q, want the command's error", stderr.String())
		}
	})

	// Two Jobs of one release, started together.
	var wg sync.WaitGroup
	codes := make([]int, 2)
	logs := make([]bytes.Buffer, 2)
	for i := range codes {
		wg.Go(func() { codes[i] = cli(context.Background(), []string{"migrate"}, getenv, io.Discard, &logs[i], nil) })
	}
	wg.Wait()
	for i, code := range codes {
		if code != 0 {
			t.Fatalf("concurrent run %d: exit code %d, want 0 (%s)", i, code, logs[i].String())
		}
	}
	if v := schemaVersion(t, env["DATABASE_URL"]); v != db.LatestVersion() {
		t.Fatalf("schema version: got %d, want %d", v, db.LatestVersion())
	}
	if months, years := horizon(t, env["DATABASE_URL"]); months < db.MinAuditMonths || years < db.MinLedgerYears {
		t.Fatalf("partition horizon: got %d months and %d years, want at least %d and %d", months, years, db.MinAuditMonths, db.MinLedgerYears)
	}

	var again bytes.Buffer
	if code := cli(context.Background(), []string{"migrate"}, getenv, io.Discard, &again, nil); code != 0 {
		t.Fatalf("second run: exit code %d, want 0 (%s)", code, again.String())
	}
	if !strings.Contains(again.String(), `"count":0`) {
		t.Errorf("second run log: got %q, want no migration applied", again.String())
	}
}

func schemaVersion(t *testing.T, url string) int {
	t.Helper()
	v, err := db.CurrentVersion(context.Background(), connect(t, url))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func horizon(t *testing.T, url string) (months, years int) {
	t.Helper()
	months, years, err := db.Horizon(context.Background(), connect(t, url), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return months, years
}

// connect opens a connection to url, closed when the test ends.
func connect(t *testing.T, url string) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(ctx) }) // what the test read is checked; closing is best-effort
	return c
}
