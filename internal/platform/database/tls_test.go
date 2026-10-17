package database_test

import (
	"context"
	"crypto/tls"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf/tlstest"
)

// TestRequireVerifiedTLS: production accepts only connection strings on
// which every host pgx may try is reached over TLS that verifies the
// server's certificate and name (ADR-0033). pgx's default, prefer, falls
// back to plaintext; require and verify-ca do not check the name.
func TestRequireVerifiedTLS(t *testing.T) {
	t.Setenv("PGSSLMODE", "")
	const plaintext, unverified = "a connection may be made without TLS", "the server's certificate or name is not verified"
	for _, c := range []struct {
		name, url, want string
	}{
		{"verify-full", "postgres://u:p@db.example.com/erp?sslmode=verify-full", ""},
		{"verify-full on several hosts", "postgres://u:p@db1.example.com,db2.example.com/erp?sslmode=verify-full", ""},
		{"default", "postgres://u:p@db.example.com/erp", plaintext},
		{"disable", "postgres://u:p@db.example.com/erp?sslmode=disable", plaintext},
		{"allow", "postgres://u:p@db.example.com/erp?sslmode=allow", plaintext},
		{"prefer", "postgres://u:p@db.example.com/erp?sslmode=prefer", plaintext},
		{"require", "postgres://u:p@db.example.com/erp?sslmode=require", unverified},
		{"verify-ca", "postgres://u:p@db.example.com/erp?sslmode=verify-ca", unverified},
		{"keyword form", "host=db.example.com user=u sslmode=require", unverified},
		{"unparseable", "postgres://u:p@db.example.com/erp?sslmode=sometimes", "database: "},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := database.RequireVerifiedTLS(c.url)
			switch {
			case c.want == "" && err != nil:
				t.Errorf("got %v, want accepted", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Errorf("got %v, want an error containing %q", err, c.want)
			}
		})
	}
	// The environment counts as pgx reads it: PGSSLMODE decides when the
	// string names no mode.
	t.Run("PGSSLMODE", func(t *testing.T) {
		t.Setenv("PGSSLMODE", "verify-full")
		if err := database.RequireVerifiedTLS("postgres://u:p@db.example.com/erp"); err != nil {
			t.Errorf("got %v, want accepted", err)
		}
	})
}

// TestOpenOverVerifiedTLS: a pool opened with sslmode=verify-full reaches
// the database through TLS, and refuses a server whose certificate its
// root does not sign.
func TestOpenOverVerifiedTLS(t *testing.T) {
	u := pgtest.TLS(t, pgtest.URL(t))
	if err := database.RequireVerifiedTLS(u); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pool, err := database.Open(ctx, u, database.DefaultSettings)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.WithConn(ctx, func(c *pgx.Conn) error {
		if _, ok := c.PgConn().Conn().(*tls.Conn); !ok {
			t.Errorf("connection: got %T, want *tls.Conn", c.PgConn().Conn())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Another authority as sslrootcert: the proxy's certificate is not
	// trusted, and pgx does not fall back.
	other := tlstest.WriteFile(t, t.TempDir(), "other.crt", tlstest.NewCA(t).PEM)
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	q.Set("sslrootcert", other)
	parsed.RawQuery = q.Encode()
	if _, err := database.Open(ctx, parsed.String(), database.DefaultSettings); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("open against an untrusted server: got %v, want a certificate error", err)
	}
}
