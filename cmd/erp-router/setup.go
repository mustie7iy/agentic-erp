package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/platform/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
)

// command runs the router with no arguments, or `erp-router
// setup-directory`, and returns the process exit code.
func command(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer, started func(api, admin net.Listener)) int {
	switch {
	case len(args) == 0:
		return cli(ctx, getenv, stderr, started)
	case args[0] == "setup-directory" && len(args) == 1:
		return setupCmd(ctx, getenv, stderr)
	}
	fmt.Fprintf(stderr, "erp-router: got %q; usage: erp-router [setup-directory]\n", args)
	return 2
}

// setupSettings are setup-directory's: the directory as its owner, the RDS
// master user, and the clients' logins with their passwords.
type setupSettings struct {
	adminURL string // ERP_DIRECTORY_ADMIN_URL, a value or a reference
	logins   []directory.Login
}

// The logins' default names, and the settings naming each login and its
// password (a reference: env: or file:).
var setupLogins = []struct{ role, user, password, name string }{
	{directory.RouterRole, "ERP_DIRECTORY_ROUTER_USER", "ERP_DIRECTORY_ROUTER_PASSWORD", "erp_router_login"},
	{directory.CellRole, "ERP_DIRECTORY_CELL_USER", "ERP_DIRECTORY_CELL_PASSWORD", "erp_cell_login"},
	{directory.WebRole, "ERP_DIRECTORY_WEB_USER", "ERP_DIRECTORY_WEB_PASSWORD", "erp_web_login"},
}

func loadSetup(getenv func(string) string) (setupSettings, error) {
	var errs []error
	local := secrets.Local{Getenv: getenv}
	resolve := func(name string) string {
		v, err := secrets.Value(context.Background(), local, getenv(name))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		} else if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
		return v
	}
	env := getenv("ERP_ENV")
	if err := config.CheckEnvironment(env); err != nil {
		errs = append(errs, err)
	}
	s := setupSettings{adminURL: resolve("ERP_DIRECTORY_ADMIN_URL")}
	if s.adminURL != "" && env == config.Production {
		if err := config.CheckDatabaseTLS("ERP_DIRECTORY_ADMIN_URL", s.adminURL); err != nil {
			errs = append(errs, err)
		}
	}
	for _, l := range setupLogins {
		name := getenv(l.user)
		if name == "" {
			name = l.name
		}
		s.logins = append(s.logins, directory.Login{Name: name, Password: resolve(l.password), Role: l.role})
	}
	return s, errors.Join(errs...)
}

// serverDatabases are the directory's server's other databases, whose
// PUBLIC CONNECT and TEMPORARY setup-directory takes back: a variable for
// the tests alone, whose server is shared with every package's.
var serverDatabases = directory.ServerDatabases

// setupCmd makes the directory's schema, its privilege roles and the
// router's, the cells' and erp-web's logins, takes PUBLIC's rights on the
// server's other databases, then exits:
//
//	erp-router setup-directory
//
// It is what the erp-global chart's pre-install and pre-upgrade hook Job
// runs, once per release, with the master credentials, which no service of
// the platform holds (T72h); each login's password is set again, so a
// changed one takes effect with the release. Runs started together
// serialize on the schema's advisory lock.
func setupCmd(ctx context.Context, getenv func(string) string, stderr io.Writer) int {
	s, err := loadSetup(getenv)
	if err != nil {
		fmt.Fprintln(stderr, "erp-router setup-directory: configuration:", err)
		return 2
	}
	if err := directory.Setup(ctx, s.adminURL, serverDatabases(), s.logins...); err != nil {
		fmt.Fprintln(stderr, "erp-router setup-directory:", err)
		return 1
	}
	return 0
}
