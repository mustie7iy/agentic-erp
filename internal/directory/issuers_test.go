package directory_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// claims lists the directory's issuer claims as "issuer audience tenant cell".
func claims(t *testing.T, dir *directory.Postgres) []string {
	t.Helper()
	var out []string
	if err := dir.Pool.InTx(context.Background(), database.Scope{ReadOnly: true}, func(ctx context.Context, tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT issuer || ' ' || audience || ' ' || tenant_id || ' ' || cell FROM directory.issuer ORDER BY 1`)
		var err error
		out, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// age makes every claim older than ClaimGrace, so a round may give it up.
func age(t *testing.T, dir *directory.Postgres) {
	t.Helper()
	if err := dir.Pool.InTx(context.Background(), database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE directory.issuer SET updated_at = now() - interval '1 hour'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// cellProviders is the server's wiring of Sync.Providers.
func cellProviders(ctx context.Context, tx pgx.Tx) ([]directory.Issuer, error) {
	keys, err := identity.ActiveIdentityProviders(ctx, tx)
	out := make([]directory.Issuer, len(keys))
	for i, k := range keys {
		out[i] = directory.Issuer{Issuer: k.Issuer, Audience: k.Audience, TenantID: k.TenantID, ProviderID: k.ID}
	}
	return out, err
}

func claim(issuer, audience string, tenant uuid.UUID, cell string) string {
	return issuer + " " + audience + " " + tenant.String() + " " + cell
}

// TestIssuerClaims: an active issuer and audience belong to one tenant
// across cells (ADR-0031). The directory keeps them in a database of its
// own: it lists every claim.
func TestIssuerClaims(t *testing.T) {
	ctx := context.Background()
	dir, err := directory.Open(ctx, pgtest.EmptyDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	a, b := uuid.New(), uuid.New()
	x := directory.Issuer{Issuer: "https://x.example.com", Audience: "erp", TenantID: a, Cell: "c1", ProviderID: uuid.New()}
	if err := dir.ClaimIssuer(ctx, x); err != nil {
		t.Fatalf("claim: %v", err)
	}
	t.Run("the same tenant claims again, from its new cell", func(t *testing.T) {
		moved := x
		moved.Cell = "c2"
		if err := dir.ClaimIssuer(ctx, moved); err != nil {
			t.Fatalf("claim again: %v", err)
		}
		if got := claims(t, dir); !slices.Equal(got, []string{claim(x.Issuer, "erp", a, "c2")}) {
			t.Errorf("claims: got %v, want x of a on c2", got)
		}
	})
	t.Run("another tenant may not", func(t *testing.T) {
		taken := x
		taken.TenantID, taken.ProviderID = b, uuid.New()
		if err := dir.ClaimIssuer(ctx, taken); !errors.Is(err, directory.ErrIssuerTaken) {
			t.Errorf("claim of another tenant: got %v, want ErrIssuerTaken", err)
		}
		if err := dir.ReleaseIssuer(ctx, taken); err != nil {
			t.Fatal(err)
		}
		if got := claims(t, dir); len(got) != 1 {
			t.Errorf("released by another tenant: got %v, want x kept", got)
		}
	})
	t.Run("released, it is free", func(t *testing.T) {
		if err := dir.ReleaseIssuer(ctx, x); err != nil {
			t.Fatal(err)
		}
		taken := x
		taken.TenantID, taken.Cell, taken.ProviderID = b, "c2", uuid.New()
		if err := dir.ClaimIssuer(ctx, taken); err != nil {
			t.Errorf("claim after the release: got %v, want claimed", err)
		}
	})
	t.Run("a cell's round replaces its claims", func(t *testing.T) {
		y := directory.Issuer{Issuer: "https://y.example.com", Audience: "erp", TenantID: a, Cell: "c1"}
		z := directory.Issuer{Issuer: "https://z.example.com", Audience: "erp", TenantID: a, Cell: "c1"}
		if taken, err := dir.SyncIssuers(ctx, "c1", []directory.Issuer{y, z}); err != nil || taken != 0 {
			t.Fatalf("round: %d taken, %v", taken, err)
		}
		t.Run("a fresh claim outlives a round that does not list it yet", func(t *testing.T) {
			if _, err := dir.SyncIssuers(ctx, "c1", []directory.Issuer{y}); err != nil {
				t.Fatal(err)
			}
			if got := claims(t, dir); !slices.Contains(got, claim(z.Issuer, "erp", a, "c1")) {
				t.Errorf("claims: got %v, want z kept within the grace", got)
			}
		})
		age(t, dir)
		// x belongs to b on c2 now; c1's round listing it for a skips it and
		// leaves it, and gives up z, which it no longer lists.
		ofA := x
		ofA.TenantID = a
		taken, err := dir.SyncIssuers(ctx, "c1", []directory.Issuer{ofA, y})
		if err != nil {
			t.Fatal(err)
		}
		if taken != 1 {
			t.Errorf("taken: got %d, want 1", taken)
		}
		want := []string{claim(x.Issuer, "erp", b, "c2"), claim(y.Issuer, "erp", a, "c1")}
		if got := claims(t, dir); !slices.Equal(got, want) {
			t.Errorf("claims: got %v, want %v", got, want)
		}
		age(t, dir)
		if _, err := dir.SyncIssuers(ctx, "c1", nil); err != nil {
			t.Fatal(err)
		}
		if got := claims(t, dir); !slices.Equal(got, []string{claim(x.Issuer, "erp", b, "c2")}) {
			t.Errorf("an empty round: got %v, want only c2's claim", got)
		}
	})
}

func TestIssuerStatementFailures(t *testing.T) {
	ctx := context.Background()
	dir, err := directory.Open(ctx, pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	is := directory.Issuer{Issuer: "https://f-" + uuid.NewString() + ".example.com", Audience: "erp", TenantID: uuid.New(), Cell: "f-" + uuid.NewString()}
	pool := dir.Pool
	for name, op := range map[string]func() error{
		"claim":   func() error { return dir.ClaimIssuer(ctx, is) },
		"release": func() error { return dir.ReleaseIssuer(ctx, is) },
		"sync": func() error {
			_, err := dir.SyncIssuers(ctx, is.Cell, []directory.Issuer{is})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			for n := 1; n < 10; n++ {
				dir.InTx = func(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error {
					return pool.InTx(ctx, scope, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, &pgtest.FailingTx{Tx: tx, FailAt: n}) })
				}
				err := op()
				if err == nil {
					break
				}
				if !errors.Is(err, pgtest.ErrInjected) {
					t.Fatalf("statement %d: got %v, want %v", n, err, pgtest.ErrInjected)
				}
			}
		})
	}
	dir.InTx = nil
}

// TestSyncClaimsIssuers: the cell claims a provider's issuer when it is
// registered and gives it up when retired, and each round replaces the
// cell's claims with its tenants' active providers.
func TestSyncClaimsIssuers(t *testing.T) {
	ctx := context.Background()
	dir, err := directory.Open(ctx, pgtest.EmptyDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	var logs bytes.Buffer
	ten := pgtest.NewTenant(t)
	active, retired := "https://active-"+uuid.NewString()+".example.com", "https://retired-"+uuid.NewString()+".example.com"
	for issuer, status := range map[string]string{active: "active", retired: "retired"} {
		pgtest.Exec(t, `INSERT INTO identity.identity_provider (tenant_id, id, issuer, audience, status, retired_at, created_by, updated_by)
			VALUES ($1, gen_random_uuid(), $2, 'erp', $3, CASE WHEN $3 = 'retired' THEN now() END, $1, $1)`, ten.ID, issuer, status)
	}
	s := &directory.Sync{Pool: pgtest.Pool(t), ControlRole: "erp_control", Directory: dir, Cell: "claims-cell", APIURL: "https://claims.example.com", Providers: cellProviders, Log: slog.New(slog.NewTextHandler(&logs, nil))}
	if _, err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	t.Run("a round claims the active providers' issuers", func(t *testing.T) {
		if got := claims(t, dir); !slices.Contains(got, claim(active, "erp", ten.ID, "claims-cell")) || slices.ContainsFunc(got, func(c string) bool { return strings.HasPrefix(c, retired) }) {
			t.Errorf("claims: got %v, want %s of the tenant and not %s", got, active, retired)
		}
	})
	t.Run("a registration claims at once; another tenant's is refused", func(t *testing.T) {
		fresh := "https://fresh-" + uuid.NewString() + ".example.com"
		provider := uuid.New()
		if err := s.ClaimIssuer(ctx, ten.ID, provider, fresh, "erp"); err != nil {
			t.Fatal(err)
		}
		err := s.ClaimIssuer(ctx, uuid.New(), uuid.New(), fresh, "erp")
		if got := problem.From(err).Code; got != "identity.provider_taken" {
			t.Errorf("claim of another tenant: got %q (%v), want identity.provider_taken", got, err)
		}
		s.ReleaseIssuer(ctx, ten.ID, provider, fresh, "erp")
		if got := claims(t, dir); slices.ContainsFunc(got, func(c string) bool { return strings.HasPrefix(c, fresh) }) {
			t.Errorf("claims after the release: got %v, want %s gone", got, fresh)
		}
	})
	t.Run("a round reports claims another tenant holds", func(t *testing.T) {
		var provider uuid.UUID
		pgtest.Scan(t, `SELECT id FROM identity.identity_provider WHERE issuer = $1`, []any{active}, &provider)
		s.ReleaseIssuer(ctx, ten.ID, provider, active, "erp")
		if err := dir.ClaimIssuer(ctx, directory.Issuer{Issuer: active, Audience: "erp", TenantID: uuid.New(), Cell: "other"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(logs.String(), "held by another tenant") {
			t.Errorf("logs: got %s, want the conflict reported", logs.String())
		}
	})
	t.Run("a directory that cannot be reached", func(t *testing.T) {
		broken, _ := directory.Open(ctx, pgtest.URL(t))
		broken.Close()
		b := *s
		b.Directory = broken
		if got := problem.From(b.ClaimIssuer(ctx, ten.ID, uuid.New(), "https://down.example.com", "erp")).Status; got != 503 {
			t.Errorf("claim: got status %d, want 503", got)
		}
		b.ReleaseIssuer(ctx, ten.ID, uuid.New(), "https://down.example.com", "erp")
		if !strings.Contains(logs.String(), "directory release of an issuer failed") {
			t.Errorf("logs: got %s, want the failed release logged", logs.String())
		}
		if _, err := b.RunOnce(ctx); err == nil {
			t.Error("round with a closed directory: got success, want an error")
		}
	})
	t.Run("a directory whose issuers fail", func(t *testing.T) {
		f := *s
		f.Directory = &failingIssuers{}
		if _, err := f.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), "issuers down") {
			t.Errorf("round: got %v, want the issuers' failure", err)
		}
	})
	t.Run("reading the providers fails", func(t *testing.T) {
		f := *s
		f.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
			tx, err := f.Pool.Begin(ctx, scope)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: 2}, nil // the tenants are read first
		}
		if _, err := f.RunOnce(ctx); !errors.Is(err, pgtest.ErrInjected) {
			t.Errorf("round: got %v, want the injected failure", err)
		}
	})
	t.Run("a directory without issuers", func(t *testing.T) {
		m := *s
		m.Directory = &memory{}
		if err := m.ClaimIssuer(ctx, ten.ID, uuid.New(), "https://memory.example.com", "erp"); err != nil {
			t.Errorf("claim: got %v, want nothing to claim", err)
		}
		m.ReleaseIssuer(ctx, ten.ID, uuid.New(), "https://memory.example.com", "erp")
	})
}

// failingIssuers keeps tenants in memory and fails every issuer operation.
type failingIssuers struct{ memory }

var errIssuersDown = errors.New("issuers down")

func (*failingIssuers) ClaimIssuer(context.Context, directory.Issuer) error   { return errIssuersDown }
func (*failingIssuers) ReleaseIssuer(context.Context, directory.Issuer) error { return errIssuersDown }
func (*failingIssuers) SyncIssuers(context.Context, string, []directory.Issuer) (int, error) {
	return 0, errIssuersDown
}

// TestReleaseGivesUpOnlyItsClaim: a tenant retiring a provider while it
// registers the same issuer and audience again gives up the old
// registration's claim only, never the new one's.
func TestReleaseGivesUpOnlyItsClaim(t *testing.T) {
	ctx := context.Background()
	dir, err := directory.Open(ctx, pgtest.EmptyDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	tenant := uuid.New()
	old := directory.Issuer{Issuer: "https://again.example.com", Audience: "erp", TenantID: tenant, Cell: "c1", ProviderID: uuid.New()}
	again := old
	again.ProviderID = uuid.New()
	if err := dir.ClaimIssuer(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := dir.ClaimIssuer(ctx, again); err != nil {
		t.Fatal(err)
	}
	if err := dir.ReleaseIssuer(ctx, old); err != nil {
		t.Fatal(err)
	}
	if got := claims(t, dir); len(got) != 1 {
		t.Errorf("claims after the old registration's release: got %v, want the new one's kept", got)
	}
	if err := dir.ReleaseIssuer(ctx, again); err != nil {
		t.Fatal(err)
	}
	if got := claims(t, dir); len(got) != 0 {
		t.Errorf("claims after the new registration's release: got %v, want none", got)
	}
}
