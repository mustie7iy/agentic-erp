package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/db"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestMigrateCommand: the hook Job's command brings an empty database to
// the binary's schema, runs started together serialize on the runner's
// lock, and a later run applies nothing.
func TestMigrateCommand(t *testing.T) {
	env := environment(t, authntest.New(t), "")
	env["DATABASE_URL"] = pgtest.EmptyDatabase(t)
	env["ERP_MIGRATE_ON_START"] = "false" // the pods' setting: the command migrates regardless
	getenv := func(k string) string { return env[k] }

	t.Run("usage", func(t *testing.T) {
		cases := map[string]struct {
			args []string
			code int
			want string
		}{
			"help":     {[]string{"migrate", "-h"}, 0, ""},
			"bad flag": {[]string{"migrate", "-x"}, 2, "flag provided"},
			"argument": {[]string{"migrate", "now"}, 2, "takes no arguments"},
		}
		for name, c := range cases {
			t.Run(name, func(t *testing.T) {
				var stderr bytes.Buffer
				if got := cli(context.Background(), c.args, getenv, io.Discard, &stderr, nil); got != c.code {
					t.Errorf("exit code: got %d, want %d (%s)", got, c.code, stderr.String())
				}
				if !strings.Contains(stderr.String(), c.want) {
					t.Errorf("stderr: got %q, want %q in it", stderr.String(), c.want)
				}
			})
		}
	})

	t.Run("database down", func(t *testing.T) {
		e := map[string]string{}
		for k, v := range env {
			e[k] = v
		}
		e["DATABASE_URL"] = "postgres://nobody@127.0.0.1:1/x?connect_timeout=1"
		var stderr bytes.Buffer
		if got := cli(context.Background(), []string{"migrate"}, func(k string) string { return e[k] }, io.Discard, &stderr, nil); got != 1 {
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
	ctx := context.Background()
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }() // the version read is checked; closing is best-effort
	v, err := db.CurrentVersion(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
