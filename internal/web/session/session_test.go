package session_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/web/session"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var (
	secretA = []byte(strings.Repeat("a", 32))
	secretB = []byte(strings.Repeat("b", 32))
)

func TestSealer(t *testing.T) {
	a, err := session.NewSealer(secretA)
	if err != nil {
		t.Fatal(err)
	}
	sealed := a.Seal([]byte("id-1"), []byte("tokens"))
	if bytes.Contains(sealed, []byte("tokens")) {
		t.Fatal("sealed: holds the plaintext")
	}
	if got, err := a.Open([]byte("id-1"), sealed); err != nil || string(got) != "tokens" {
		t.Errorf("open: got %q, %v, want tokens", got, err)
	}
	if again := a.Seal([]byte("id-1"), []byte("tokens")); bytes.Equal(again, sealed) {
		t.Error("seal: got the same bytes twice, want a fresh nonce")
	}
	t.Run("rotation", func(t *testing.T) {
		rotated, err := session.NewSealer(secretB, secretA) // B current, A still opens
		if err != nil {
			t.Fatal(err)
		}
		if got, err := rotated.Open([]byte("id-1"), sealed); err != nil || string(got) != "tokens" {
			t.Errorf("old key: got %q, %v, want it opened", got, err)
		}
		b, _ := session.NewSealer(secretB)
		if _, err := b.Open([]byte("id-1"), sealed); !errors.Is(err, session.ErrUnsealed) {
			t.Errorf("a retired key: got %v, want ErrUnsealed", err)
		}
	})
	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 1
	for name, c := range map[string]struct{ aad, sealed []byte }{
		"another session": {[]byte("id-2"), sealed},
		"altered":         {[]byte("id-1"), tampered},
		"empty":           {[]byte("id-1"), nil},
		"another version": {[]byte("id-1"), append([]byte{9}, sealed[1:]...)},
		"no nonce":        {[]byte("id-1"), sealed[:12]},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := a.Open(c.aad, c.sealed); !errors.Is(err, session.ErrUnsealed) {
				t.Errorf("got %v, want ErrUnsealed", err)
			}
		})
	}
	if _, err := session.NewSealer(); err == nil {
		t.Error("no secret: got a sealer")
	}
	if _, err := session.NewSealer([]byte("short")); err == nil {
		t.Error("a short secret: got a sealer")
	}
}

// clock is a settable clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newStore is a store on a directory database of the test's own.
func newStore(t *testing.T) (*session.Store, *clock, *directory.Postgres) {
	t.Helper()
	ctx := context.Background()
	dirURL := pgtest.EmptyDatabase(t)
	dir, err := directory.Open(ctx, dirURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dir.Close)
	sealer, err := session.NewSealer(secretA)
	if err != nil {
		t.Fatal(err)
	}
	st, err := session.Open(ctx, dirURL, nil, sealer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	c := &clock{now: time.Now().Truncate(time.Microsecond)}
	st.Now = c.Now
	return st, c, dir
}

func person() session.Session {
	return session.Session{TenantID: uuid.New(), Issuer: "https://idp.example.com", Subject: "alice", LoginCode: "acme",
		Secrets: session.Secrets{AccessToken: "at", AccessExpiry: time.Now().Add(time.Hour), RefreshToken: "rt", IDToken: "it"}}
}

// TestSessions: a session is made with a fresh id and CSRF token, the
// store keeps its id's hash and its tokens sealed, it is read back, seen,
// and ends at its idle timeout or lifetime, deleted then (ADR-0052 §6).
func TestSessions(t *testing.T) {
	ctx := context.Background()
	st, c, dir := newStore(t)
	s, err := st.Create(ctx, person())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.ID) != 43 || len(s.CSRF) != 43 || s.ID == s.CSRF {
		t.Errorf("secrets: got id %q, csrf %q, want two 256-bit values", s.ID, s.CSRF)
	}
	var stored []byte
	var hashed bool
	if err := dir.Pool.InTx(ctx, database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT sealed, id_hash = $1 FROM web.session`, session.Hash(s.ID)).Scan(&stored, &hashed)
	}); err != nil {
		t.Fatal(err)
	}
	if !hashed || bytes.Contains(stored, []byte("rt")) || bytes.Contains(stored, []byte(s.CSRF)) {
		t.Errorf("stored: hashed id %v, sealed %q, want the id's hash and nothing readable", hashed, stored)
	}
	got, err := st.Load(ctx, s.ID)
	if err != nil || got.Subject != "alice" || got.RefreshToken != "rt" || got.CSRF != s.CSRF || got.Version != 1 {
		t.Fatalf("load: got %+v, %v", got, err)
	}
	t.Run("seen", func(t *testing.T) {
		c.Advance(2 * time.Minute)
		got, err := st.Load(ctx, s.ID)
		if err != nil || !got.LastSeen.Equal(c.Now()) {
			t.Errorf("load: got last seen %v, %v, want now", got.LastSeen, err)
		}
	})
	t.Run("idle", func(t *testing.T) {
		c.Advance(session.DefaultIdle)
		if _, err := st.Load(ctx, s.ID); !errors.Is(err, session.ErrNoSession) {
			t.Errorf("got %v, want ErrNoSession", err)
		}
		if n := count(t, dir); n != 0 {
			t.Errorf("rows: got %d, want the idle session deleted", n)
		}
	})
	t.Run("lifetime", func(t *testing.T) {
		s, err := st.Create(ctx, person())
		if err != nil {
			t.Fatal(err)
		}
		for range 4 {
			c.Advance(5 * time.Hour)
			if _, err := st.Load(ctx, s.ID); err != nil {
				t.Fatalf("within the lifetime: %v", err)
			}
		}
		c.Advance(5 * time.Hour)
		if _, err := st.Load(ctx, s.ID); !errors.Is(err, session.ErrNoSession) {
			t.Errorf("past it: got %v, want ErrNoSession", err)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		for _, id := range []string{"", "nobody"} {
			if _, err := st.Load(ctx, id); !errors.Is(err, session.ErrNoSession) {
				t.Errorf("%q: got %v, want ErrNoSession", id, err)
			}
		}
	})
	t.Run("a retired sealing key", func(t *testing.T) {
		s, err := st.Create(ctx, person())
		if err != nil {
			t.Fatal(err)
		}
		other := *st
		other.Sealer, _ = session.NewSealer(secretB)
		if _, err := other.Load(ctx, s.ID); !errors.Is(err, session.ErrNoSession) {
			t.Errorf("got %v, want ErrNoSession", err)
		}
	})
	t.Run("secrets that do not read", func(t *testing.T) {
		id := session.NewSecret()
		if err := dir.Pool.InTx(ctx, database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO web.session (id_hash, tenant_id, issuer, subject, login_code, created_at, last_seen_at, expires_at, sealed)
				VALUES ($1, $2, 'i', 's', 'c', $3, $3, $4, $5)`, session.Hash(id), uuid.New(), c.Now(), c.Now().Add(time.Hour), st.Sealer.Seal(session.Hash(id), []byte("not json")))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Load(ctx, id); !errors.Is(err, session.ErrNoSession) {
			t.Errorf("got %v, want ErrNoSession", err)
		}
	})
	t.Run("deleted", func(t *testing.T) {
		s, err := st.Create(ctx, person())
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Delete(ctx, s.ID); err != nil {
			t.Fatal(err)
		}
		if err := st.Delete(ctx, s.ID); err != nil {
			t.Errorf("again: %v", err)
		}
		if _, err := st.Load(ctx, s.ID); !errors.Is(err, session.ErrNoSession) {
			t.Errorf("got %v, want ErrNoSession", err)
		}
	})
}

func count(t *testing.T, dir *directory.Postgres) int {
	t.Helper()
	var n int
	if err := dir.Pool.InTx(context.Background(), database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM web.session`).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestRenewOnce: concurrent requests of one session renew it once; the
// others wait and take what the first renewed.
func TestRenewOnce(t *testing.T) {
	ctx := context.Background()
	st, _, _ := newStore(t)
	s, err := st.Create(ctx, person())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	renewals := 0
	fresh := func(sec session.Secrets) bool { return sec.AccessToken != "at" }
	renew := func(_ context.Context, sec session.Secrets) (session.Secrets, error) {
		mu.Lock()
		defer mu.Unlock()
		renewals++
		sec.AccessToken = "at-2"
		return sec, nil
	}
	var wg sync.WaitGroup
	got := make([]session.Session, 8)
	errs := make([]error, 8)
	for i := range got {
		wg.Go(func() { got[i], errs[i] = st.Renew(ctx, s, fresh, renew) })
	}
	wg.Wait()
	for i := range got {
		if errs[i] != nil || got[i].AccessToken != "at-2" || got[i].Version != 2 {
			t.Errorf("renewal %d: got %q version %d, %v, want at-2 version 2", i, got[i].AccessToken, got[i].Version, errs[i])
		}
	}
	if renewals != 1 {
		t.Errorf("renewals: got %d, want 1", renewals)
	}
	t.Run("a refusal changes nothing", func(t *testing.T) {
		refused := errors.New("refused")
		if _, err := st.Renew(ctx, s, func(session.Secrets) bool { return false }, func(context.Context, session.Secrets) (session.Secrets, error) {
			return session.Secrets{}, refused
		}); !errors.Is(err, refused) {
			t.Errorf("got %v, want the refusal", err)
		}
		if again, _ := st.Load(ctx, s.ID); again.AccessToken != "at-2" {
			t.Errorf("after: got %q, want at-2 kept", again.AccessToken)
		}
	})
	t.Run("gone", func(t *testing.T) {
		if _, err := st.Renew(ctx, session.Session{ID: "gone"}, fresh, renew); !errors.Is(err, session.ErrNoSession) {
			t.Errorf("got %v, want ErrNoSession", err)
		}
	})
}

// TestSweep deletes the sessions past their lifetime or idle timeout, and
// only those.
func TestSweep(t *testing.T) {
	ctx := context.Background()
	st, c, dir := newStore(t)
	st.Idle, st.MaxAge, st.TouchEvery = time.Hour, 2*time.Hour, time.Second
	if _, err := st.Create(ctx, person()); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Hour)
	keep, err := st.Create(ctx, person())
	if err != nil {
		t.Fatal(err)
	}
	if n, err := st.Sweep(ctx); err != nil || n != 1 {
		t.Errorf("sweep: got %d, %v, want 1", n, err)
	}
	if n, err := st.Sweep(ctx); err != nil || n != 0 {
		t.Errorf("again: got %d, %v, want 0", n, err)
	}
	if _, err := st.Load(ctx, keep.ID); err != nil || count(t, dir) != 1 {
		t.Errorf("the live session: %v", err)
	}
}

// TestClients finds web clients by login code as erp_web, remembering
// answers, unknown codes too, for CacheTTL.
func TestClients(t *testing.T) {
	ctx := context.Background()
	st, c, dir := newStore(t)
	is := directory.Issuer{Issuer: "https://idp.example.com", Audience: "erp", TenantID: uuid.New(), Cell: "eu-1", ProviderID: uuid.New(),
		Web: &directory.WebClient{LoginCode: "acme", ClientID: "erp-web", AuthMethod: "private_key_jwt", CredentialRef: "file:/k/acme.pem", Scope: "openid erp"}}
	if err := dir.ClaimIssuer(ctx, is); err != nil {
		t.Fatal(err)
	}
	if err := dir.ClaimWebClient(ctx, is); err != nil {
		t.Fatal(err)
	}
	clients := &session.Clients{Store: st}
	got, err := clients.Find(ctx, "acme")
	want := session.Client{LoginCode: "acme", Issuer: is.Issuer, Audience: "erp", TenantID: is.TenantID, ClientID: "erp-web", AuthMethod: "private_key_jwt",
		CredentialRef: "file:/k/acme.pem", Scope: "openid erp"}
	if err != nil || got != want {
		t.Fatalf("find: got %+v, %v, want %+v", got, err, want)
	}
	if _, err := clients.Find(ctx, "nobody"); !errors.Is(err, session.ErrUnknownCode) {
		t.Errorf("unknown: got %v, want ErrUnknownCode", err)
	}
	if err := dir.ReleaseIssuer(ctx, is); err != nil {
		t.Fatal(err)
	}
	if _, err := clients.Find(ctx, "acme"); err != nil {
		t.Errorf("cached: got %v, want the remembered client", err)
	}
	c.Advance(session.CacheTTL)
	if _, err := clients.Find(ctx, "acme"); !errors.Is(err, session.ErrUnknownCode) {
		t.Errorf("after the TTL: got %v, want ErrUnknownCode", err)
	}
}

// failing makes the store's nth statement fail.
func failing(st *session.Store, n int) {
	pool := st.Pool
	st.InTx = func(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error {
		return pool.InTx(ctx, scope, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, &pgtest.FailingTx{Tx: tx, FailAt: n}) })
	}
}

// TestStoreDatabaseFailures: every statement of every store operation
// failing in turn is reported.
func TestStoreDatabaseFailures(t *testing.T) {
	ctx := context.Background()
	st, c, _ := newStore(t)
	s, err := st.Create(ctx, person())
	if err != nil {
		t.Fatal(err)
	}
	ended, err := st.Create(ctx, person())
	if err != nil {
		t.Fatal(err)
	}
	for name, op := range map[string]func() error{
		"create": func() error { _, err := st.Create(ctx, person()); return err },
		"load":   func() error { _, err := st.Load(ctx, s.ID); return err },
		"touch": func() error {
			c.Advance(2 * time.Minute)
			_, err := st.Load(ctx, s.ID)
			return err
		},
		"delete ended": func() error {
			c.Advance(session.DefaultIdle)
			defer c.Advance(-session.DefaultIdle)
			_, err := st.Load(ctx, ended.ID)
			if errors.Is(err, session.ErrNoSession) {
				return nil
			}
			return err
		},
		"renew": func() error {
			_, err := st.Renew(ctx, s, func(session.Secrets) bool { return false }, func(_ context.Context, sec session.Secrets) (session.Secrets, error) { return sec, nil })
			return err
		},
		"delete": func() error { return st.Delete(ctx, "x") },
		"sweep":  func() error { _, err := st.Sweep(ctx); return err },
		"check":  func() error { return st.Check(ctx) },
		"find": func() error {
			_, err := (&session.Clients{Store: st}).Find(ctx, "acme")
			if errors.Is(err, session.ErrUnknownCode) {
				return nil
			}
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			for n := 1; n < 10; n++ {
				failing(st, n)
				err := op()
				st.InTx = nil
				if err == nil {
					return
				}
				if !errors.Is(err, pgtest.ErrInjected) {
					t.Fatalf("statement %d: got %v, want the injected failure", n, err)
				}
			}
			t.Fatal("never succeeded")
		})
	}
}

// TestOpenRefuses: a database that cannot be reached, or without the web
// schema, is refused at startup.
func TestOpenRefuses(t *testing.T) {
	ctx := context.Background()
	sealer, _ := session.NewSealer(secretA)
	if _, err := session.Open(ctx, "postgres://nobody@127.0.0.1:1/x?connect_timeout=1", nil, sealer); err == nil {
		t.Error("unreachable: got a store")
	}
	if _, err := session.Open(ctx, pgtest.EmptyDatabase(t), nil, sealer); err == nil || !strings.Contains(err.Error(), "web schema") {
		t.Errorf("no web schema: got %v, want it refused", err)
	}
}
