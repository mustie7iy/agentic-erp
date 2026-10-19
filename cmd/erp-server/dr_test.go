package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/db"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestDRCommand marks a drill tenant and verifies the cell with the
// markers, on a database of its own: verify reads every tenant, and the
// package database holds what the other tests left.
func TestDRCommand(t *testing.T) {
	ctx := context.Background()
	u := pgtest.EmptyDatabase(t)
	conn, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(ctx, conn); err != nil {
		t.Fatal(err)
	}
	ten := uuid.New()
	if _, err := conn.Exec(ctx, `INSERT INTO tenancy.tenant (id, name, region, group_currency, status) VALUES ($1, 'Drill', 'eu-west', 'EUR', 'active')`, ten); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx) // set up; the commands open their own connections
	env := map[string]string{"DATABASE_URL": u, "ERP_ENV": "development"}
	dir := t.TempDir()
	mark := func(name string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := cli(ctx, []string{"dr", "mark", "-tenant", ten.String(), "-name", name}, func(k string) string { return env[k] }, &stdout, &stderr, nil); code != 0 {
			t.Fatalf("mark %s: %d %s", name, code, stderr.String())
		}
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, stdout.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	before := mark("before")
	after := filepath.Join(dir, "after.json")
	if err := os.WriteFile(after, []byte(`{"tenant_id":"`+ten.String()+`","name":"after"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeJSON, err := os.ReadFile(before)
	if err != nil {
		t.Fatal(err)
	}
	// The database behind TLS, its authority given as PEM rather than a
	// file (the dr-copies drill's Fargate task).
	tlsURL, err := url.Parse(pgtest.TLS(t, u))
	if err != nil {
		t.Fatal(err)
	}
	q := tlsURL.Query()
	caPEM, err := os.ReadFile(q.Get("sslrootcert"))
	if err != nil {
		t.Fatal(err)
	}
	q.Del("sslrootcert")
	tlsURL.RawQuery = q.Encode()
	junk := filepath.Join(dir, "junk.json")
	if err := os.WriteFile(junk, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		args []string
		env  map[string]string
		code int
		want string
	}{
		{"passes", []string{"dr", "verify", "-restored", before, "-lost", after}, nil, 0, `"problems": []`},
		{"against the old database", []string{"dr", "verify"}, map[string]string{"ERP_DR_OLD_DATABASE_URL": u}, 0, `"problems": []`},
		{"markers from the environment, the database's authority as PEM", []string{"dr", "verify", "-restored", "env:ERP_DR_RESTORED", "-lost", after},
			map[string]string{"ERP_DR_RESTORED": string(beforeJSON), "ERP_ENV": "production", "DATABASE_URL": tlsURL.String(), "ERP_DR_DATABASE_CA": string(caPEM)}, 0, `"problems": []`},
		{"verify-full without the authority", []string{"dr", "verify"}, map[string]string{"ERP_ENV": "production", "DATABASE_URL": tlsURL.String()}, 1, "erp-server dr verify"},
		{"an authority for a database that is no URL", []string{"dr", "verify"}, map[string]string{"DATABASE_URL": "file:/etc/erp/db", "ERP_DR_DATABASE_CA": "pem"}, 2, "needs DATABASE_URL as a postgres:// URL"},
		{"an empty marker variable", []string{"dr", "verify", "-restored", "env:ERP_DR_RESTORED"}, nil, 1, "ERP_DR_RESTORED is empty"},
		{"fails a restore past its point", []string{"dr", "verify", "-lost", before}, nil, 1, "1 checks failed"},
		{"no subcommand", []string{"dr"}, nil, 2, "mark, verify or reconcile"},
		{"an unknown subcommand", []string{"dr", "restore"}, nil, 2, "mark, verify or reconcile"},
		{"an unknown flag", []string{"dr", "verify", "-x"}, nil, 2, "flag provided but not defined"},
		{"help", []string{"dr", "verify", "-h"}, nil, 0, "-restored"},
		{"a bad tenant", []string{"dr", "mark", "-tenant", "x", "-name", "n"}, nil, 1, "-tenant"},
		{"a missing restored marker", []string{"dr", "verify", "-restored", filepath.Join(dir, "none")}, nil, 1, "marker:"},
		{"a lost marker that is not JSON", []string{"dr", "verify", "-lost", junk}, nil, 1, "junk.json"},
		{"an old database out of reach", []string{"dr", "verify"}, map[string]string{"ERP_DR_OLD_DATABASE_URL": "postgres://postgres@127.0.0.1:1/x?connect_timeout=1"}, 1, "the old database"},
		{"a database out of reach", []string{"dr", "verify"}, map[string]string{"DATABASE_URL": "postgres://postgres@127.0.0.1:1/x?connect_timeout=1"}, 1, "erp-server dr verify"},
		{"no database", []string{"dr", "verify"}, map[string]string{"DATABASE_URL": ""}, 2, "DATABASE_URL is required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := map[string]string{}
			for k, v := range env {
				e[k] = v
			}
			for k, v := range c.env {
				e[k] = v
			}
			var stdout, stderr bytes.Buffer
			code := cli(ctx, c.args, func(k string) string { return e[k] }, &stdout, &stderr, nil)
			if code != c.code {
				t.Errorf("exit: got %d, want %d (%s)", code, c.code, stderr.String())
			}
			if got := stdout.String() + stderr.String(); !strings.Contains(got, c.want) {
				t.Errorf("output: got %s, want it to contain %q", got, c.want)
			}
		})
	}
}

// TestDRDatabaseCAUnwritable: an authority that cannot be written stops
// the command before it connects, and leaves no directory behind. It sets
// TMPDIR and replaces drWriteCA, so it must not run in parallel.
func TestDRDatabaseCAUnwritable(t *testing.T) {
	env := map[string]string{"DATABASE_URL": "postgres://u@db.example/erp", "ERP_DR_DATABASE_CA": "pem"}
	getenv := func(k string) string { return env[k] }
	t.Run("no temporary directory", func(t *testing.T) {
		t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "none"))
		if _, _, err := drDatabaseCA(getenv); err == nil || !strings.Contains(err.Error(), "ERP_DR_DATABASE_CA") {
			t.Errorf("got %v, want an ERP_DR_DATABASE_CA error", err)
		}
	})
	t.Run("the file not written", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		drWriteCA = func(string, []byte, fs.FileMode) error { return errors.New("disk full") }
		t.Cleanup(func() { drWriteCA = os.WriteFile })
		var stdout, stderr bytes.Buffer
		if code := cli(context.Background(), []string{"dr", "verify"}, getenv, &stdout, &stderr, nil); code != 2 || !strings.Contains(stderr.String(), "disk full") {
			t.Errorf("got %d %s, want 2 and the write's error", code, stderr.String())
		}
		left, err := os.ReadDir(tmp)
		if err != nil {
			t.Fatal(err)
		}
		if len(left) != 0 {
			t.Errorf("left behind: got %d entries, want none", len(left))
		}
	})
}
