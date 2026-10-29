package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf/tlstest"
)

// setupEnvironment is setup-directory's settings for a database of the
// test's own, with logins of the test's own (roles are the server's).
func setupEnvironment(t *testing.T) (map[string]string, map[string]pgtest.Login) {
	t.Helper()
	dir := t.TempDir()
	env := map[string]string{"ERP_ENV": "development", "ERP_DIRECTORY_ADMIN_URL": "file:" + tlstest.WriteFile(t, dir, "admin-url", []byte(pgtest.EmptyDatabase(t)+"\n"))}
	logins := map[string]pgtest.Login{}
	for _, l := range setupLogins {
		c := pgtest.NewLogin(t)
		logins[l.role] = c
		env[l.user] = c.Name
		env[l.password] = "file:" + tlstest.WriteFile(t, dir, l.role, []byte(c.Password))
	}
	return env, logins
}

// TestSetupDirectory: erp-router setup-directory makes the directory and
// the three logins, as which the router and a cell then open it (T72h).
func TestSetupDirectory(t *testing.T) {
	ctx := context.Background()
	env, logins := setupEnvironment(t)
	var stderr bytes.Buffer
	if code := command(ctx, []string{"setup-directory"}, func(k string) string { return env[k] }, &stderr, nil); code != 0 {
		t.Fatalf("exit: got %d (%s), want 0", code, stderr.String())
	}
	admin, _ := loadSetup(func(k string) string { return env[k] }) // loaded above
	for _, role := range []string{directory.RouterRole, directory.CellRole} {
		dir, err := directory.OpenWith(ctx, logins[role].As(admin.adminURL), nil)
		if err != nil {
			t.Errorf("%s's login: got %v, want it to open the directory", role, err)
			continue
		}
		dir.Close()
	}
	if code := command(ctx, []string{"setup-directory"}, func(k string) string { return env[k] }, &stderr, nil); code != 0 {
		t.Errorf("again: got %d (%s), want 0", code, stderr.String())
	}
}

// TestSetupDirectoryRefuses: bad settings exit 2, a failed setup 1, and
// other arguments 2.
func TestSetupDirectoryRefuses(t *testing.T) {
	ctx := context.Background()
	env, _ := setupEnvironment(t)
	with := func(k, v string) map[string]string {
		e := map[string]string{}
		for name, value := range env {
			e[name] = value
		}
		e[k] = v
		return e
	}
	for name, c := range map[string]struct {
		env  map[string]string
		args []string
		code int
		want string
	}{
		"no admin URL":            {with("ERP_DIRECTORY_ADMIN_URL", ""), nil, 2, "ERP_DIRECTORY_ADMIN_URL is required"},
		"no password":             {with("ERP_DIRECTORY_CELL_PASSWORD", ""), nil, 2, "ERP_DIRECTORY_CELL_PASSWORD is required"},
		"unreadable":              {with("ERP_DIRECTORY_WEB_PASSWORD", "file:"+filepath.Join(t.TempDir(), "missing")), nil, 2, "ERP_DIRECTORY_WEB_PASSWORD"},
		"no environment":          {with("ERP_ENV", ""), nil, 2, "ERP_ENV"},
		"production in the clear": {with("ERP_ENV", "production"), nil, 2, "verify-full"},
		"out of reach":            {with("ERP_DIRECTORY_ADMIN_URL", "postgres://nobody@127.0.0.1:1/x?connect_timeout=1"), nil, 1, "setup"},
		"a login as owner":        {with("ERP_DIRECTORY_ROUTER_USER", directory.CellRole), nil, 1, "login"},
		"another argument":        {env, []string{"setup-directory", "now"}, 2, "usage"},
		"another command":         {env, []string{"migrate"}, 2, "usage"},
	} {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			args := c.args
			if args == nil {
				args = []string{"setup-directory"}
			}
			if code := command(ctx, args, func(k string) string { return c.env[k] }, &stderr, nil); code != c.code {
				t.Errorf("exit: got %d, want %d", code, c.code)
			}
			if !strings.Contains(stderr.String(), c.want) {
				t.Errorf("stderr: got %q, want it to name %q", stderr.String(), c.want)
			}
		})
	}
}

// TestSetupDefaults: without the user settings, the logins are the
// erp-global chart's.
func TestSetupDefaults(t *testing.T) {
	s, err := loadSetup(func(k string) string {
		return map[string]string{"ERP_ENV": "development", "ERP_DIRECTORY_ADMIN_URL": "postgres://owner@db/directory",
			"ERP_DIRECTORY_ROUTER_PASSWORD": "r", "ERP_DIRECTORY_CELL_PASSWORD": "c", "ERP_DIRECTORY_WEB_PASSWORD": "w"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []directory.Login{{Name: "erp_router_login", Password: "r", Role: directory.RouterRole},
		{Name: "erp_cell_login", Password: "c", Role: directory.CellRole}, {Name: "erp_web_login", Password: "w", Role: directory.WebRole}}
	for i, l := range s.logins {
		if l != want[i] {
			t.Errorf("login %d: got %+v, want %+v", i, l, want[i])
		}
	}
}

// TestNoCommandRuns: without arguments the router itself runs, here
// refusing a configuration without its directory.
func TestNoCommandRuns(t *testing.T) {
	var stderr bytes.Buffer
	if code := command(context.Background(), nil, func(string) string { return "" }, &stderr, nil); code != 2 || !strings.Contains(stderr.String(), "ERP_DIRECTORY_URL") {
		t.Errorf("got %d, %q, want 2 for the router's own configuration", code, stderr.String())
	}
}
