package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/db"
	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestDRReconcileCommand reconciles a cell of its own with its object
// store and a directory that places a tenant the cell lacks.
func TestDRReconcileCommand(t *testing.T) {
	ctx := context.Background()
	u := pgtest.EmptyDatabase(t)
	conn, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(ctx, conn); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx) // migrated; the command opens its own connections
	dirURL := pgtest.EmptyDatabase(t)
	dir, err := directory.Open(ctx, dirURL)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := dir.Upsert(ctx, directory.Entry{TenantID: uuid.New(), Region: "eu-west", Cell: "test", APIURL: "https://test.api.example.com", Status: directory.Active}); err != nil {
		t.Fatal(err)
	}
	env := environment(t, authntest.New(t), "")
	env["DATABASE_URL"] = u
	withDirectory := map[string]string{"ERP_DIRECTORY_URL": dirURL, "ERP_CELL_API_URL": "https://test.api.example.com", "ERP_CELL_REGION": "eu-west",
		"ERP_CELL_CONTROL_URL": "http://test-admin.internal:9090"}
	unreachable := "postgres://postgres@127.0.0.1:1/x?connect_timeout=1"
	for _, c := range []struct {
		name string
		args []string
		env  map[string]string
		code int
		want string
	}{
		{"passes", nil, nil, 0, `"problems": []`},
		{"a tenant the directory places here and the cell lacks", nil, withDirectory, 1, "which lacks it"},
		{"no object store", nil, map[string]string{"ERP_OBJECT_DIR": ""}, 1, "no object storage"},
		{"arguments", []string{"x"}, nil, 2, "takes no arguments"},
		{"a configuration refused", nil, map[string]string{"ERP_PAGE_TOKEN_KEY": ""}, 2, "configuration"},
		{"a database out of reach", nil, map[string]string{"DATABASE_URL": unreachable}, 1, "the restored database"},
		{"a directory out of reach", nil, map[string]string{"ERP_DIRECTORY_URL": unreachable, "ERP_CELL_API_URL": "https://test.api.example.com",
			"ERP_CELL_REGION": "eu-west", "ERP_CELL_CONTROL_URL": "http://test-admin.internal:9090"}, 1, "directory"},
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
			code := cli(ctx, append([]string{"dr", "reconcile"}, c.args...), func(k string) string { return e[k] }, &stdout, &stderr, nil)
			if code != c.code {
				t.Errorf("exit: got %d, want %d (%s)", code, c.code, stderr.String())
			}
			if got := stdout.String() + stderr.String(); !strings.Contains(got, c.want) {
				t.Errorf("output: got %s, want it to contain %q", got, c.want)
			}
		})
	}
}
