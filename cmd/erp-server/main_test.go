package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/google/uuid"
	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"

	"github.com/mustie7iy/agentic-erp/internal/offboarding"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore/blobstoretest"
	"github.com/mustie7iy/agentic-erp/internal/platform/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt/kmstest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/natsbus"
	"github.com/mustie7iy/agentic-erp/internal/platform/natsbus/natstest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/workflow/workflowtest"
	"github.com/mustie7iy/agentic-erp/internal/provisioning"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// environment is a valid configuration on the package database.
func environment(t *testing.T, is *authntest.Issuer, natsURL string) map[string]string {
	t.Helper()
	issuers, _ := json.Marshal([]any{is.Config()})
	return map[string]string{
		"DATABASE_URL":         pgtest.URL(t),
		"ERP_ENV":              "development",
		"ERP_HTTP_ADDR":        "127.0.0.1:0",
		"ERP_ADMIN_ADDR":       "127.0.0.1:0",
		"ERP_CELL":             "test",
		"ERP_NATS_URL":         natsURL,
		"ERP_OIDC_ISSUERS":     string(issuers),
		"ERP_PAGE_TOKEN_KEY":   base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))),
		"ERP_FIELD_KEY":        base64.StdEncoding.EncodeToString([]byte(strings.Repeat("f", 32))),
		"ERP_MIGRATE_ON_START": "true",
		"ERP_SHUTDOWN_TIMEOUT": "5s",
		"ERP_OBJECT_DIR":       t.TempDir(),
	}
}

func load(t *testing.T, env map[string]string) config.Config {
	t.Helper()
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestServesTheAPIAndShutsDownCleanly(t *testing.T) {
	is := authntest.New(t)
	natsURL := natstest.Server(t)
	env := environment(t, is, natsURL)
	env["ANTHROPIC_API_KEY"] = "test" // invoice capture is wired; nothing calls the model here
	env["ERP_BANK_OUTBOX_DIR"] = t.TempDir()
	maintenanceInterval = 10 * time.Millisecond // the partition maintenance runs while the server serves
	defer func() { maintenanceInterval = 6 * time.Hour }()
	ten := pgtest.NewTenant(t, "platform.operation.read", "platform.operation.cancel")
	pgtest.Exec(t, `INSERT INTO platform.operation (tenant_id, id, kind, status, created_by, updated_by) VALUES ($1, gen_random_uuid(), 'payments.propose', 'running', $2, $2)`,
		ten.ID, ten.Human.ID)

	ctx, cancel := context.WithCancel(context.Background())
	addrs := make(chan [2]string, 1)
	done := make(chan int, 1)
	var logs pgtest.SyncBuffer // the server logs on its goroutines while the test reads
	go func() {
		done <- cli(ctx, nil, func(k string) string { return env[k] }, io.Discard, &logs, func(api, admin net.Listener) {
			addrs <- [2]string{api.Addr().String(), admin.Addr().String()}
		})
	}()
	exit := 0
	stop := sync.OnceFunc(func() { cancel(); exit = <-done })
	// A failed wait below must stop the server before maintenanceInterval
	// is reset, or the server outlives the test and races the reset.
	defer stop()
	a := <-addrs
	get := func(u, token string) (int, map[string]any) {
		req, _ := http.NewRequestWithContext(t.Context(), "GET", u, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		readJSON(t, resp, &body)
		return resp.StatusCode, body
	}
	if code, _ := get("http://"+a[1]+"/readyz", ""); code != 200 {
		t.Fatalf("readyz %d", code)
	}
	// The workers' backlogs are read on each scrape of the admin metrics (a
	// read that outlasts its timeout on a loaded machine is left out of that
	// scrape, so the test scrapes until every backlog is there).
	var scraped []byte
	pgtest.Eventually(t, 30*time.Second, func() bool {
		mresp, err := http.DefaultClient.Do(request(t, "GET", "http://"+a[1]+"/metrics", ""))
		if err != nil {
			t.Fatal(err)
		}
		scraped, _ = io.ReadAll(mresp.Body)
		_ = mresp.Body.Close() // the scrape is already read; a close error cannot change what is asserted
		return strings.Contains(string(scraped), "erp_outbox_backlog ") && strings.Contains(string(scraped), "erp_projection_backlog 0") &&
			strings.Contains(string(scraped), "erp_payment_proposals_queued 0") && strings.Contains(string(scraped), "erp_simulations_queued 0")
	}, "metrics lack a backlog:\n%s", lazy(func() string { return string(scraped) }))
	for _, want := range []string{"erp_outbox_backlog ", "erp_projection_backlog 0", "erp_payment_proposals_queued 0", "erp_simulations_queued 0",
		"erp_outbox_lag_seconds ", "erp_projection_lag_seconds 0"} {
		if !strings.Contains(string(scraped), want) {
			t.Errorf("metrics lack %q:\n%s", want, scraped)
		}
	}
	token := is.Token(t, ten.ID, ten.Human.Subject, nil)
	code, page := get("http://"+a[0]+"/v1/operations", token)
	if code != 200 || len(page["items"].([]any)) != 1 {
		t.Fatalf("list %d %v", code, page)
	}
	op := page["items"].([]any)[0].(map[string]any)

	// Cancelling publishes OperationCompleted through the relay to JetStream.
	req, _ := http.NewRequestWithContext(t.Context(), "POST", "http://"+a[0]+"/v1/operations/"+op["id"].(string)+":cancel", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", "key-server-test-1")
	req.Header.Set("If-Match", `"1"`)
	cresp, err := http.DefaultClient.Do(req)
	if err != nil || cresp.StatusCode != 200 {
		t.Fatal(cresp.StatusCode, err)
	}
	_ = cresp.Body.Close() // only the status is asserted; the body is not needed
	nc, _ := nats.Connect(natsURL)
	defer nc.Close()
	js, _ := jetstream.New(nc)
	stream, _ := js.Stream(context.Background(), natsbus.StreamName("test", "platform"))
	subject := fmt.Sprintf("erp.test.platform.%s.operation_completed", ten.ID)
	var relayed *jetstream.RawStreamMsg
	pgtest.Eventually(t, 30*time.Second, func() bool {
		m, err := stream.GetLastMsgForSubject(context.Background(), subject)
		relayed = m
		return err == nil
	}, "event %s not relayed", subject)
	if relayed.Header.Get("ce-subject") != op["id"] {
		t.Fatalf("event subject %s", relayed.Header.Get("ce-subject"))
	}

	stop()
	if exit != 0 {
		t.Fatalf("exit %d\n%s", exit, logs.String())
	}
}

func TestReadiness(t *testing.T) {
	ctx := context.Background()
	if err := ready(pgtest.Pool(t))(ctx); err != nil {
		t.Fatal(err)
	}
	emptyURL := pgtest.EmptyDatabase(t)
	empty, err := database.Open(ctx, emptyURL, database.DefaultSettings)
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	if err := ready(empty)(ctx); err == nil {
		t.Fatal("database without migrations reported ready")
	}
	// A schema older than the binary is not ready.
	c, err := pgx.Connect(ctx, emptyURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }() // best-effort: the database is dropped at cleanup anyway
	for _, sql := range []string{
		"CREATE TABLE public.erp_schema_migration (version integer PRIMARY KEY, name text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())",
		"INSERT INTO public.erp_schema_migration VALUES (1, '000001_foundation', now())",
	} {
		if _, err := c.Exec(ctx, sql); err != nil {
			t.Fatalf("old schema setup: %v", err)
		}
	}
	pgtest.WantErr(t, ready(empty)(ctx), "schema version 1")
	// A partition horizon that got too short is not ready either; maintenance restores it.
	shortURL := pgtest.EmptyDatabase(t)
	short, err := database.Open(ctx, shortURL, database.DefaultSettings)
	if err != nil {
		t.Fatal(err)
	}
	defer short.Close()
	if err := migrate(ctx, short, quiet()); err != nil {
		t.Fatal(err)
	}
	sc, err := pgx.Connect(ctx, shortURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sc.Close(ctx) }() // best-effort: the database is dropped at cleanup anyway
	if _, err := sc.Exec(ctx, `DO $$ DECLARE r record; BEGIN FOR r IN SELECT relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'audit' AND relname ~ '^audit_event_[0-9]{6}$' AND substr(relname, 13) > to_char(now(), 'YYYYMM')
		LOOP EXECUTE format('DROP TABLE audit.%I', r.relname); END LOOP; END $$`); err != nil {
		t.Fatal(err)
	}
	pgtest.WantErr(t, ready(short)(ctx), "partition horizon")
	if err := partitions(ctx, short, quiet()); err != nil {
		t.Fatal(err)
	}
	if err := ready(short)(ctx); err != nil {
		t.Fatalf("after maintenance: %v", err)
	}
	// A database without the schema fails the partition check and the maintenance.
	if err := partitions(ctx, empty, quiet()); err == nil {
		t.Fatal("maintenance without the schema")
	}
}

func TestMaintenanceRetries(t *testing.T) {
	var logs pgtest.SyncBuffer // the server logs on its goroutines while the test reads
	ctx, cancel := context.WithCancel(context.Background())
	runs := 0
	done := make(chan struct{})
	go func() {
		maintain(ctx, time.Millisecond, func(context.Context) error {
			runs++
			switch runs {
			case 1: // a panic fails the round, not the maintenance for good
				panic("partition boom")
			case 3:
				cancel()
			}
			return errors.New("not now")
		}, slog.New(slog.NewTextHandler(&logs, nil)))
		close(done)
	}()
	<-done
	if runs != 3 {
		t.Errorf("runs: got %d, want 3", runs)
	}
	if n := strings.Count(logs.String(), "partition maintenance failed"); n != 2 {
		t.Errorf("logged failures: got %d, want 2:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), `error="panic: partition boom"`) {
		t.Errorf("logs: got %s, want the panic as a failed round", logs.String())
	}
	if !strings.Contains(logs.String(), "stack=") {
		t.Errorf("logs: got %s, want the panic's stack", logs.String())
	}
	if n := strings.Count(logs.String(), "\n"); n != 2 { // the panic is logged once, as its round's failure
		t.Errorf("log records: got %d, want 2:\n%s", n, logs.String())
	}
}

func TestStartupFailures(t *testing.T) {
	is := authntest.New(t)
	ctx := context.Background()
	busy, _ := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	defer func() { _ = busy.Close() }() // best-effort cleanup of the port the cases collide with
	cases := map[string]func(t *testing.T, cfg *config.Config){
		"otlp endpoint": func(_ *testing.T, c *config.Config) { c.OTLPEndpoint = "not a url" },
		"database": func(_ *testing.T, c *config.Config) {
			c.DatabaseURL = "postgres://nobody@127.0.0.1:1/x?connect_timeout=1"
		},
		"issuer": func(_ *testing.T, c *config.Config) { c.Issuers[0].URL = "http://127.0.0.1:1" },
		"operator issuer": func(_ *testing.T, c *config.Config) {
			c.OperatorIssuers = []authn.Issuer{{URL: "http://127.0.0.1:1", Audience: "erp-api"}}
		},
		"page key":  func(_ *testing.T, c *config.Config) { c.PageTokenKey = []byte("short") },
		"field key": func(_ *testing.T, c *config.Config) { c.FieldKey = []byte("short") },
		"master key": func(_ *testing.T, c *config.Config) {
			c.FieldKeys = []config.MasterKey{{ID: "k", Secret: []byte("short")}}
		},
		"nats":              func(_ *testing.T, c *config.Config) { c.NATSURL = "nats://127.0.0.1:1" },
		"jetstream missing": func(t *testing.T, c *config.Config) { t.Helper(); c.NATSURL = natstest.Plain(t) },
		"api port":          func(_ *testing.T, c *config.Config) { c.HTTPAddr = busy.Addr().String() },
		"admin port":        func(_ *testing.T, c *config.Config) { c.AdminAddr = busy.Addr().String() },
		"migrations":        func(t *testing.T, c *config.Config) { t.Helper(); c.DatabaseURL = limitedUser(t) },
		"partitions": func(t *testing.T, c *config.Config) {
			t.Helper()
			c.DatabaseURL, c.MigrateOnStart = pgtest.EmptyDatabase(t), false
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := load(t, environment(t, is, ""))
			mutate(t, &cfg)
			if err := run(ctx, cfg, quiet(), nil); err == nil {
				t.Errorf("got the server started, want a startup failure")
			}
		})
	}
	// A meter provider that cannot create instruments fails the API. (The
	// global delegate cannot be set back, so a no-op provider takes its place.)
	otel.SetMeterProvider(failingProvider{})
	defer otel.SetMeterProvider(noop.NewMeterProvider())
	if err := run(ctx, load(t, environment(t, is, "")), quiet(), nil); err == nil {
		t.Error("broken meter provider: started")
	}
}

// TestAStartupFailureStopsTheWorkersFirst: when the bus cannot be connected,
// its streams cannot be made or a listener cannot be opened, the workers
// (and, for a listener, the relay) have already started; run returns only
// once every one of them stopped, so the pool it then closes is no longer in
// use.
func TestAStartupFailureStopsTheWorkersFirst(t *testing.T) {
	is := authntest.New(t)
	var lc net.ListenConfig
	busy, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = busy.Close() }) // only holds the port; nothing is written
	cases := []struct {
		name   string
		nats   func() string
		mutate func(*config.Config)
		want   string // the step that failed, so the case cannot pass on an earlier failure
	}{
		{"bus unreachable", func() string { return "nats://127.0.0.1:1" }, func(*config.Config) {}, "natsbus: connect"},
		{"streams refused", func() string { return natstest.Plain(t) }, func(*config.Config) {}, "natsbus: stream"},
		{"api port in use", func() string { return natstest.Server(t) }, func(c *config.Config) { c.HTTPAddr = busy.Addr().String() }, "erp-server: listen on"},
		{"admin port in use", func() string { return natstest.Server(t) }, func(c *config.Config) { c.AdminAddr = busy.Addr().String() }, "erp-server: admin listen on"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var s *server
			startWorkersHook = func(srv *server, runCtx context.Context) { s = srv; srv.startWorkers(runCtx) }
			defer func() { startWorkersHook = (*server).startWorkers }()
			cfg := load(t, environment(t, is, c.nats()))
			c.mutate(&cfg)
			pgtest.WantErr(t, run(context.Background(), cfg, quiet(), nil), c.want)
			if s == nil {
				t.Fatal("got no workers started, want them started before the failure")
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			for _, name := range []string{"projector", "simulator", "proposer", "releaser", "maintenance", "expirer"} {
				if _, ok := s.running[name]; !ok {
					t.Errorf("%s: got not started, want started before the failure", name)
				}
			}
			for name, running := range s.running {
				if running {
					t.Errorf("%s: got still running after run returned, want stopped", name)
				}
			}
		})
	}
}

func TestAListenerFailureStopsTheServer(t *testing.T) {
	is := authntest.New(t)
	cfg := load(t, environment(t, is, ""))
	err := run(context.Background(), cfg, quiet(), func(api, _ net.Listener) {
		// Closing provokes the serve error under test; a failed close shows as a nil error below.
		_ = api.Close()
	})
	if err == nil {
		t.Fatal("closed listener did not stop the server")
	}
}

func TestExitCodes(t *testing.T) {
	var stderr bytes.Buffer
	if code := cli(context.Background(), nil, func(string) string { return "" }, io.Discard, &stderr, nil); code != 2 || !strings.Contains(stderr.String(), "DATABASE_URL") {
		t.Fatalf("bad configuration: %d %s", code, stderr.String())
	}
	is := authntest.New(t)
	env := environment(t, is, "")
	// ERP_ENV has no default: without it the server does not start, and
	// says what to set.
	delete(env, "ERP_ENV")
	stderr.Reset()
	if code := cli(context.Background(), nil, func(k string) string { return env[k] }, io.Discard, &stderr, nil); code != 2 {
		t.Fatalf("no ERP_ENV: got exit %d (%q), want 2", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "ERP_ENV is required: set ERP_ENV=development") {
		t.Fatalf("no ERP_ENV: got %q, want it naming ERP_ENV and what to set", stderr.String())
	}
	env = environment(t, is, "")
	env["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] = "not a url"
	stderr.Reset()
	if code := cli(context.Background(), nil, func(k string) string { return env[k] }, io.Discard, &stderr, nil); code != 1 {
		t.Fatalf("run failure: %d", code)
	}
	// main exits with cli's code; here the configuration is empty.
	exited := -1
	exit = func(code int) { exited = code }
	defer func() { exit = os.Exit }()
	for _, k := range []string{"DATABASE_URL", "ERP_OIDC_ISSUERS", "ERP_PAGE_TOKEN_KEY", "ERP_FIELD_KEY"} {
		t.Setenv(k, "")
	}
	main()
	if exited != 2 {
		t.Fatalf("main exit %d", exited)
	}
}

// --- helpers ---------------------------------------------------------------

// limitedUser is a login that may connect but not create anything.
func limitedUser(t *testing.T) string {
	t.Helper()
	pgtest.Exec(t, `DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'erp_test_limited') THEN
		CREATE ROLE erp_test_limited LOGIN PASSWORD 'limited'; END IF; END $$`)
	u, _ := url.Parse(pgtest.EmptyDatabase(t))
	u.User = url.UserPassword("erp_test_limited", "limited")
	return u.String()
}

type failingMeter struct{ noop.Meter }

func (failingMeter) Int64Counter(string, ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	return nil, errors.New("no counters")
}

type failingProvider struct{ noop.MeterProvider }

func (failingProvider) Meter(string, ...metric.MeterOption) metric.Meter { return failingMeter{} }

// TestProvisionAndDevelopmentLogin is the operator's path to a usable
// tenant: erp-server provision creates it, the development issuer mints its
// administrator a token, and the administrator works through the API.
func TestProvisionAndDevelopmentLogin(t *testing.T) {
	anchorInterval = 50 * time.Millisecond
	defer func() { anchorInterval = time.Minute }()
	env := environment(t, authntest.New(t), "")
	delete(env, "ERP_OIDC_ISSUERS")
	env["ERP_DEV_ISSUER"] = "true"
	file := configFile(t)
	s := &devLogin{env: env, getenv: func(k string) string { return env[k] }, file: file, logs: &pgtest.SyncBuffer{},
		args: []string{"provision", "-name", "Acme", "-region", "eu-west", "-currency", "EUR", "-admin-subject", "idp|ada", "-admin-name", "Ada", "-admin-email", "ada@example.com", "-config", file}}
	// Each step builds on the ones before it, so the first failure stops the rest.
	if !t.Run("provision", s.provision) {
		t.FailNow()
	}
	if !t.Run("provision refusals", s.provisionRefusals) {
		t.FailNow()
	}
	// Should a step after serve fail, shutdown does not run: stop the server
	// when the test ends. Cancelling twice is harmless.
	t.Cleanup(func() {
		if s.cancel != nil {
			s.cancel()
		}
	})
	if !t.Run("serve", s.serve) {
		t.FailNow()
	}
	if !t.Run("administrator token", s.administratorToken) {
		t.FailNow()
	}
	if !t.Run("operator token", s.operatorToken) {
		t.FailNow()
	}
	if !t.Run("anchoring", s.anchoring) {
		t.FailNow()
	}
	if !t.Run("shutdown", s.shutdown) {
		t.FailNow()
	}
}

// devLogin is the state the steps of TestProvisionAndDevelopmentLogin share.
type devLogin struct {
	env     map[string]string
	getenv  func(string) string
	file    string   // the configuration document
	args    []string // the provision command
	created struct {
		TenantID        string `json:"tenant_id"`
		AdministratorID string `json:"administrator_id"`
		Version         int    `json:"configuration_version"`
	}
	ctx    context.Context // the server's; cancel stops it
	cancel context.CancelFunc
	done   chan int           // the server's exit code
	logs   *pgtest.SyncBuffer // the server logs on its goroutines while the test reads
	a      [2]string          // the API's and the admin listener's addresses
	minted string             // the administrator's token
}

// provision: erp-server provision creates the tenant.
func (s *devLogin) provision(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := cli(context.Background(), s.args, s.getenv, &stdout, &stderr, nil); code != 0 {
		t.Fatalf("provision: %d %s", code, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &s.created); err != nil || s.created.Version != 1 {
		t.Fatalf("%v %s", err, stdout.String())
	}
}

// provisionRefusals: bad commands, and without a reachable database
// provisioning fails after the configuration was accepted.
func (s *devLogin) provisionRefusals(t *testing.T) {
	env, getenv, file, args := s.env, s.getenv, s.file, s.args
	var stderr bytes.Buffer
	for name, c := range map[string]struct {
		args []string
		code int
		want string
	}{
		"help":        {[]string{"provision", "-h"}, 0, ""},
		"bad flag":    {[]string{"provision", "-x"}, 2, "flag provided"},
		"no config":   {[]string{"provision", "-name", "x"}, 1, "-config is required"},
		"no file":     {[]string{"provision", "-config", "/no/such.yaml"}, 1, "no such file"},
		"invalid":     {[]string{"provision", "-config", file}, 1, "tenancy.invalid_tenant"},
		"unknown cmd": {[]string{"serve"}, 2, "unknown command"},
	} {
		t.Run(name, func(t *testing.T) {
			stderr.Reset()
			if code := cli(context.Background(), c.args, getenv, io.Discard, &stderr, nil); code != c.code || !strings.Contains(stderr.String(), c.want) {
				t.Errorf("got %d %s, want %d and %q", code, stderr.String(), c.code, c.want)
			}
		})
	}
	down := map[string]string{}
	for k, v := range env {
		down[k] = v
	}
	down["DATABASE_URL"] = "postgres://nobody@127.0.0.1:1/x?connect_timeout=1"
	if code := cli(context.Background(), args, func(k string) string { return down[k] }, io.Discard, &stderr, nil); code != 1 {
		t.Fatalf("database down: %d", code)
	}
}

// serve: the server with the development issuer starts serving.
func (s *devLogin) serve(t *testing.T) {
	s.ctx, s.cancel = context.WithCancel(context.Background())
	addrs := make(chan [2]string, 1)
	s.done = make(chan int, 1)
	go func() {
		s.done <- cli(s.ctx, nil, s.getenv, io.Discard, s.logs, func(api, admin net.Listener) {
			addrs <- [2]string{api.Addr().String(), admin.Addr().String()}
		})
	}()
	select {
	case s.a = <-addrs:
	case code := <-s.done:
		t.Fatalf("the server stopped with %d before serving:\n%s", code, s.logs.String())
	}
}

// administratorToken: a minted token works on the API.
func (s *devLogin) administratorToken(t *testing.T) {
	a, created := s.a, s.created
	mint, err := http.DefaultClient.Do(request(t, "POST", "http://"+a[1]+"/dev/token", `{"tenant_id": "`+created.TenantID+`", "subject": "idp|ada"}`))
	if err != nil {
		t.Fatal(err)
	}
	var minted struct{ Token string }
	readJSON(t, mint, &minted)
	if mint.StatusCode != 200 || minted.Token == "" {
		t.Fatalf("mint: %d", mint.StatusCode)
	}
	s.minted = minted.Token
	req, _ := http.NewRequestWithContext(t.Context(), "GET", "http://"+a[0]+"/v1/config-versions?is_active=true", nil)
	req.Header.Set("Authorization", "Bearer "+minted.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var page map[string]any
	readJSON(t, resp, &page)
	if resp.StatusCode != 200 || len(page["items"].([]any)) != 1 {
		t.Fatalf("as the administrator: %d %v", resp.StatusCode, page)
	}
}

// operatorToken: an operator token (no tenant) works on the control plane
// and nowhere else: the tenant is suspended, refused at the API's door, and
// resumed.
func (s *devLogin) operatorToken(t *testing.T) {
	a, created := s.a, s.created
	mint, err := http.DefaultClient.Do(request(t, "POST", "http://"+a[1]+"/dev/token", `{"subject": "ops|root"}`))
	if err != nil {
		t.Fatal(err)
	}
	var operator struct{ Token string }
	readJSON(t, mint, &operator)
	control := func(method, path, ifMatch string) (int, map[string]any) {
		req, _ := http.NewRequestWithContext(t.Context(), method, "http://"+a[1]+"/control/v1"+path, nil)
		req.Header.Set("Authorization", "Bearer "+operator.Token)
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		readJSON(t, resp, &m)
		return resp.StatusCode, m
	}
	if code, m := control("GET", "/tenants/"+created.TenantID, ""); code != 200 || m["status"] != "active" {
		t.Fatalf("control get: %d %v", code, m)
	}
	req, _ := http.NewRequestWithContext(t.Context(), "GET", "http://"+a[0]+"/v1/roles", nil)
	req.Header.Set("Authorization", "Bearer "+operator.Token)
	if code := status(t, req); code != 401 {
		t.Fatalf("operator on the public API: got %d, want 401", code)
	}
	if code, m := control("POST", "/tenants/"+created.TenantID+":suspend", `"1"`); code != 200 || m["status"] != "suspended" {
		t.Fatalf("suspend: %d %v", code, m)
	}
	req, _ = http.NewRequestWithContext(t.Context(), "GET", "http://"+a[0]+"/v1/roles", nil)
	req.Header.Set("Authorization", "Bearer "+s.minted)
	if code := status(t, req); code != 403 {
		t.Fatalf("suspended tenant on the public API: got %d, want 403", code)
	}
	if code, m := control("POST", "/tenants/"+created.TenantID+":resume", `"2"`); code != 200 || m["status"] != "active" {
		t.Fatalf("resume: %d %v", code, m)
	}
}

// anchoring: the anchoring worker wrote the tenant's chain head to the
// object store.
func (s *devLogin) anchoring(t *testing.T) {
	ctx, env, created := s.ctx, s.env, s.created
	conn, err := pgx.Connect(ctx, env["DATABASE_URL"])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }() // best-effort: a read-only connection
	pgtest.Eventually(t, 30*time.Second, func() bool {
		var anchored int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM audit.anchor WHERE tenant_id = $1`, created.TenantID).Scan(&anchored); err != nil {
			t.Fatalf("count anchors: %v", err)
		}
		return anchored > 0
	}, "no anchor written")
	if objects, _ := filepath.Glob(filepath.Join(env["ERP_OBJECT_DIR"], "tenants", created.TenantID, "audit", "anchors", "*.json")); len(objects) == 0 {
		t.Fatal("no anchor object")
	}
}

// shutdown: the server stops cleanly and logged what it did.
func (s *devLogin) shutdown(t *testing.T) {
	logs := s.logs
	s.cancel()
	if code := <-s.done; code != 0 {
		t.Fatalf("exit: got %d, want 0\n%s", code, logs.String())
	}
	for _, want := range []string{"ERP_DEV_ISSUER", "control plane served", "suspendTenant"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs: got none with %q, want it logged\n%s", want, logs.String())
		}
	}
}

// TestProductionOperatorWithoutRoles: the server passes ERP_ENV through to
// the operator verifier, so in production an operator token without a roles
// claim holds no role on the control plane (ADR-0027, amended), while one
// that carries the role is let through. The environment is production's:
// an object store, the default audit verification, no development issuer.
func TestProductionOperatorWithoutRoles(t *testing.T) {
	is := authntest.New(t)
	env := environment(t, is, "")
	env["ERP_ENV"] = "production"
	env["ERP_OPERATOR_ISSUERS"] = env["ERP_OIDC_ISSUERS"]
	ctx, cancel := context.WithCancel(context.Background())
	var logs pgtest.SyncBuffer // the server logs on its goroutines while the test reads
	admin := make(chan string, 1)
	done := make(chan int, 1)
	go func() {
		done <- cli(ctx, nil, func(k string) string { return env[k] }, io.Discard, &logs, func(_, a net.Listener) { admin <- a.Addr().String() })
	}()
	stop := sync.OnceValue(func() int { cancel(); return <-done })
	defer stop()
	var base string
	select {
	case a := <-admin:
		base = "http://" + a + "/control/v1/tenants/"
	case code := <-done:
		done <- code // the deferred stop reads the exit code again
		t.Fatalf("stopped before serving: %d\n%s", code, logs.String())
	}
	get := func(extra map[string]any) int {
		req, _ := http.NewRequestWithContext(t.Context(), "GET", base+uuid.NewString(), nil)
		req.Header.Set("Authorization", "Bearer "+is.Token(t, uuid.Nil, "ops|alice", extra))
		return status(t, req)
	}
	if got := get(map[string]any{"tenant_id": nil}); got != http.StatusForbidden {
		t.Errorf("no roles claim: got %d, want 403", got)
	}
	if got := get(map[string]any{"tenant_id": nil, "roles": []string{"erp:read"}}); got != http.StatusNotFound {
		t.Errorf("with erp:read: got %d, want 404 (an unknown tenant, past the role check)", got)
	}
	if code := stop(); code != 0 {
		t.Errorf("stop: exit code got %d, want 0\n%s", code, logs.String())
	}
}

// TestObjectStores: without object storage the server warns and runs; with
// an S3-compatible store it anchors chain heads there.
func TestObjectStores(t *testing.T) {
	is := authntest.New(t)
	ctx := context.Background()
	serve := func(env map[string]string) string {
		cfg := load(t, env)
		runCtx, cancel := context.WithCancel(ctx)
		var logs pgtest.SyncBuffer // the server logs on its goroutines while the test reads
		started := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- run(runCtx, cfg, slog.New(slog.NewTextHandler(&logs, nil)), func(net.Listener, net.Listener) { close(started) })
		}()
		select {
		case <-started:
		case err := <-done:
			t.Fatalf("stopped before serving: %v", err)
		}
		return func() string { cancel(); <-done; return logs.String() }()
	}
	env := environment(t, is, "")
	delete(env, "ERP_OBJECT_DIR")
	env["ERP_AUDIT_VERIFY_INTERVAL"] = "0"
	if logs := serve(env); !strings.Contains(logs, "no object storage configured") || !strings.Contains(logs, "ERP_AUDIT_VERIFY_INTERVAL is 0") {
		t.Fatalf("logs: %s", logs)
	}
	delete(env, "ERP_AUDIT_VERIFY_INTERVAL")
	// A tenant with a chain head and no anchor yet.
	ten := pgtest.NewTenant(t)
	pgtest.Exec(t, `INSERT INTO audit.chain_head (tenant_id, seq, hash) VALUES ($1, 1, decode(repeat('ab', 32), 'hex'))`, ten.ID)
	s3 := blobstoretest.New(t)
	env = environment(t, is, "")
	delete(env, "ERP_OBJECT_DIR")
	env["ERP_OBJECT_S3_ENDPOINT"], env["ERP_OBJECT_S3_BUCKET"], env["ERP_OBJECT_S3_REGION"] = s3.URL, blobstoretest.Bucket, blobstoretest.Region
	env["ERP_OBJECT_S3_ACCESS_KEY"], env["ERP_OBJECT_S3_SECRET_KEY"] = blobstoretest.AccessKey, blobstoretest.SecretKey
	anchorInterval = 20 * time.Millisecond
	defer func() { anchorInterval = time.Minute }()
	anchored := func(key string) func() bool { return func() bool { _, ok := s3.Object(key); return ok } }
	stop := background(load(t, env), quiet())
	defer func() { _ = stop() }() // stops a server a failed wait left running before anchorInterval is reset; the wait's failure is the one reported
	key := "tenants/" + ten.ID.String() + "/audit/anchors/000000000001.json"
	pgtest.Eventually(t, 30*time.Second, anchored(key), "no anchor %s in S3: %v", key, lazy(func() string { return fmt.Sprint(s3.Keys()) }))
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	o, ok := s3.Object(key)
	if !ok {
		t.Fatalf("anchor in S3: got none, want %s among %v", key, s3.Keys())
	}
	if o.LockMode != "COMPLIANCE" {
		t.Errorf("anchor lock mode: got %q, want COMPLIANCE", o.LockMode)
	}
	if o.ContentType != "application/json" {
		t.Errorf("anchor content type: got %q, want application/json", o.ContentType)
	}
	// The same store reached with the deployment's ambient credentials (the AWS_* environment), no keys in settings.
	ten2 := pgtest.NewTenant(t)
	pgtest.Exec(t, `INSERT INTO audit.chain_head (tenant_id, seq, hash) VALUES ($1, 1, decode(repeat('cd', 32), 'hex'))`, ten2.ID)
	delete(env, "ERP_OBJECT_S3_ACCESS_KEY")
	delete(env, "ERP_OBJECT_S3_SECRET_KEY")
	env["AWS_ACCESS_KEY_ID"], env["AWS_SECRET_ACCESS_KEY"], env["AWS_SESSION_TOKEN"] = blobstoretest.AccessKey, blobstoretest.SecretKey, "ambient-session"
	stopAmbient := background(load(t, env), quiet())
	defer func() { _ = stopAmbient() }() // as stop above
	key = "tenants/" + ten2.ID.String() + "/audit/anchors/000000000001.json"
	pgtest.Eventually(t, 30*time.Second, anchored(key), "no anchor %s in S3 with ambient credentials: %v", key, lazy(func() string { return fmt.Sprint(s3.Keys()) }))
	if err := stopAmbient(); err != nil {
		t.Fatal(err)
	}
	if _, ok := s3.Object(key); !ok {
		t.Fatalf("anchor with ambient credentials: %v", s3.Keys())
	}
}

// TestKeyRotationPolicy: with an age set the server rotates old keys.
func TestKeyRotationPolicy(t *testing.T) {
	is := authntest.New(t)
	env := environment(t, is, "")
	env["ERP_KEY_ROTATION_AGE"] = "1ns"
	getenv := func(k string) string { return env[k] }
	file := configFile(t)
	var stdout, stderr bytes.Buffer
	if code := cli(context.Background(), []string{"provision", "-name", "Old", "-region", "eu-west", "-currency", "EUR", "-admin-subject", "idp|old", "-admin-name", "Old", "-config", file}, getenv, &stdout, &stderr, nil); code != 0 {
		t.Fatalf("provision: %d %s", code, stderr.String())
	}
	var created struct {
		TenantID string `json:"tenant_id"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &created); err != nil {
		t.Fatalf("provision output %q: %v", stdout.String(), err)
	}
	rotationInterval = 20 * time.Millisecond
	defer func() { rotationInterval = time.Hour }()
	var logs pgtest.SyncBuffer // the server logs on its goroutines while the test reads
	stop := background(load(t, env), slog.New(slog.NewTextHandler(&logs, nil)))
	// A failed wait must stop the server before rotationInterval is reset,
	// or the server outlives the test and races the reset.
	defer func() { _ = stop() }() // the wait's failure is the one reported
	// One connection for every check: pgtest.Admin opens a new one per call
	// and keeps it to the end of the test, which a slow round turned into
	// more connections than the database allows.
	admin := pgtest.Admin(t)
	n := 0
	pgtest.Eventually(t, 30*time.Second, func() bool {
		if err := admin.QueryRow(context.Background(), `SELECT count(*) FROM tenancy.tenant_key WHERE tenant_id = $1`, created.TenantID).Scan(&n); err != nil {
			t.Fatalf("count keys: %v", err)
		}
		return n >= 2
	}, "%v keys: the policy did not rotate; logs:\n%s", lazy(func() string { return fmt.Sprint(n) }), &logs)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

// background runs the server on cfg until stop is called; stop cancels it,
// waits for it and returns run's error, and may be called again, so a
// deferred stop ends a server that a failed wait left running before the
// test resets the intervals it read.
func background(cfg config.Config, log *slog.Logger) (stop func() error) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, log, nil) }()
	return sync.OnceValue(func() error { cancel(); return <-done })
}

// lazy formats as what f returns when the message is built, so a failed
// wait reports the last value its condition saw, not the one when it began.
type lazy func() string

func (f lazy) String() string { return f() }

// TestImportCommand brings an erased tenant back from its export.
func TestImportCommand(t *testing.T) {
	is := authntest.New(t)
	env := environment(t, is, "")
	getenv := func(k string) string { return env[k] }
	file := configFile(t)
	var stdout, stderr bytes.Buffer
	if code := cli(context.Background(), []string{"provision", "-name", "Back", "-region", "eu-west", "-currency", "EUR", "-admin-subject", "idp|back", "-admin-name", "Back", "-config", file}, getenv, &stdout, &stderr, nil); code != 0 {
		t.Fatalf("provision: %d %s", code, stderr.String())
	}
	var created struct {
		TenantID uuid.UUID `json:"tenant_id"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &created); err != nil {
		t.Fatalf("provision output %q: %v", stdout.String(), err)
	}
	// Offboarded, exported and erased by the worker; the trail wiped as a destination cell would find it.
	pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'offboarded', version = version + 1 WHERE id = $1`, created.TenantID)
	pool := pgtest.Pool(t)
	store := blobstore.Dir{Root: env["ERP_OBJECT_DIR"]}
	w := &offboarding.Worker{Pool: pool, Store: store, AppRole: "erp_app", WorkerRole: "erp_worker", Retention: 24 * time.Hour, Log: quiet()} // locked exports survive the erasure
	ex, err := w.Export(context.Background(), created.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Erase(context.Background(), created.TenantID, offboarding.Actor, nil); err != nil {
		t.Fatal(err)
	}
	wipeTrail(t, created.TenantID)
	stdout.Reset()
	if code := cli(context.Background(), []string{"import", "-manifest", ex.Ref}, getenv, &stdout, &stderr, nil); code != 0 || !strings.Contains(stdout.String(), `"identity.principal"`) {
		t.Fatalf("import: %d %s %s", code, stdout.String(), stderr.String())
	}
	var status string
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT status FROM tenancy.tenant WHERE id = $1`, created.TenantID).Scan(&status); err != nil {
		t.Fatalf("tenant status: %v", err)
	}
	if status != "active" {
		t.Fatalf("tenant after the import: %s", status)
	}
	for name, c := range map[string]struct {
		args []string
		env  func(map[string]string)
		code int
		want string
	}{
		"help":        {[]string{"import", "-h"}, nil, 0, ""},
		"bad flag":    {[]string{"import", "-x"}, nil, 2, "flag provided"},
		"no manifest": {[]string{"import"}, nil, 1, "-manifest is required"},
		"live tenant": {[]string{"import", "-manifest", ex.Ref}, nil, 1, "not erased"},
		"no store":    {[]string{"import", "-manifest", ex.Ref}, func(e map[string]string) { delete(e, "ERP_OBJECT_DIR") }, 1, "no object storage"},
		"db down":     {[]string{"import", "-manifest", ex.Ref}, func(e map[string]string) { e["DATABASE_URL"] = "postgres://nobody@127.0.0.1:1/x?connect_timeout=1" }, 1, "connect"},
	} {
		t.Run(name, func(t *testing.T) {
			e := map[string]string{}
			for k, v := range env {
				e[k] = v
			}
			if c.env != nil {
				c.env(e)
			}
			stderr.Reset()
			if code := cli(context.Background(), c.args, func(k string) string { return e[k] }, io.Discard, &stderr, nil); code != c.code || !strings.Contains(stderr.String(), c.want) {
				t.Errorf("got %d %s, want %d and %q", code, stderr.String(), c.code, c.want)
			}
		})
	}
}

// TestRewrapCommand puts a KMS master in front of the ring and moves a
// tenant's keys onto it.
func TestRewrapCommand(t *testing.T) {
	env := environment(t, authntest.New(t), "")
	getenv := func(k string) string { return env[k] }
	file := configFile(t)
	var stdout, stderr bytes.Buffer
	// A tenant provisioned on the legacy master v1.
	args := []string{"provision", "-name", "Acme", "-region", "eu-west", "-currency", "EUR", "-admin-subject", "idp|ada", "-admin-name", "Ada", "-config", file}
	if code := cli(context.Background(), args, getenv, &stdout, &stderr, nil); code != 0 {
		t.Fatalf("provision: %d %s", code, stderr.String())
	}
	before := countKeys(t, "v1")
	if before == 0 {
		t.Fatal("no key wrapped by v1")
	}
	// The KMS in front. Before anything moves: usage, a KMS that cannot be reached, no database, no control role.
	kms := kmstest.New(t)
	env["ERP_FIELD_KMS_KEY"], env["ERP_FIELD_KMS_ENDPOINT"], env["ERP_FIELD_KMS_REGION"] = kmstest.KeyID, kms.URL, kmstest.Region
	env["ERP_FIELD_KMS_ACCESS_KEY"], env["ERP_FIELD_KMS_SECRET_KEY"] = kmstest.AccessKey, kmstest.SecretKey
	failing := func(name string, args []string, mutate func(env map[string]string), code int, want string) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			e := map[string]string{}
			for k, v := range env {
				e[k] = v
			}
			mutate(e)
			stderr.Reset()
			if got := cli(context.Background(), args, func(k string) string { return e[k] }, io.Discard, &stderr, nil); got != code || !strings.Contains(stderr.String(), want) {
				t.Errorf("got %d %s, want %d and %q", got, stderr.String(), code, want)
			}
		})
	}
	none := func(map[string]string) {}
	failing("help", []string{"rewrap", "-h"}, none, 0, "")
	failing("bad flag", []string{"rewrap", "-x"}, none, 2, "flag provided")
	failing("kms down", []string{"rewrap"}, func(e map[string]string) { e["ERP_FIELD_KMS_ENDPOINT"] = "http://127.0.0.1:1" }, 1, "kms Encrypt")
	failing("database down", []string{"rewrap"}, func(e map[string]string) { e["DATABASE_URL"] = "postgres://nobody@127.0.0.1:1/x?connect_timeout=1" }, 1, "connect")
	failing("control role", []string{"rewrap"}, func(e map[string]string) { e["ERP_DB_CONTROL_ROLE"] = "nobody_role" }, 1, "listing tenants")
	if n := countKeys(t, "v1"); n != before {
		t.Fatalf("%d keys on v1 after failed runs, had %d", n, before)
	}
	// Every key moves, and a second run finds nothing to move.
	var res struct {
		Master  string `json:"master_key_id"`
		Tenants int    `json:"tenants"`
		Keys    int    `json:"keys"`
	}
	for i, want := range []int{before, 0} {
		stdout.Reset()
		if code := cli(context.Background(), []string{"rewrap"}, getenv, &stdout, &stderr, nil); code != 0 {
			t.Fatalf("rewrap %d: %d %s", i, code, stderr.String())
		}
		if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
			t.Fatalf("rewrap %d output %q: %v", i, stdout.String(), err)
		}
		if res.Master != "kms" {
			t.Fatalf("rewrap %d master: got %s, want kms", i, res.Master)
		}
		if res.Keys != want {
			t.Fatalf("rewrap %d keys: got %d, want %d", i, res.Keys, want)
		}
		if res.Tenants == 0 {
			t.Fatalf("rewrap %d tenants: got 0, want some", i)
		}
	}
	if n := countKeys(t, "v1"); n != 0 {
		t.Fatalf("%d keys still on v1", n)
	}
	// A ring without the master the keys are wrapped by cannot move them.
	failing("missing master", []string{"rewrap"}, func(e map[string]string) {
		delete(e, "ERP_FIELD_KMS_KEY")
		e["ERP_FIELD_KEYS"] = "k9=" + e["ERP_FIELD_KEY"]
		delete(e, "ERP_FIELD_KEY")
	}, 1, "master key this server does not have (kms)")
}

// countKeys counts the tenant keys wrapped by a master, in the package database.
func countKeys(t *testing.T, master string) int {
	t.Helper()
	var n int
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT count(*) FROM tenancy.tenant_key WHERE master_key_id = $1`, master).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestDirectorySync: with a directory configured the server records its
// tenants there, API URL and status included. The directory is a database of
// its own, as in production: its tables in the package database would be
// tenant tables to every later export there.
func TestDirectorySync(t *testing.T) {
	is := authntest.New(t)
	env := environment(t, is, "")
	directoryURL := pgtest.EmptyDatabase(t)
	env["ERP_DIRECTORY_URL"], env["ERP_CELL_API_URL"], env["ERP_CELL_CONTROL_URL"], env["ERP_CELL_REGION"] = directoryURL, "https://test.api.example.com", "http://test-admin.internal:9090", "eu-west"
	env["ERP_OPERATOR_ISSUERS"] = env["ERP_OIDC_ISSUERS"] // the control plane claims providers in the directory
	ten := pgtest.NewTenant(t)
	directoryInterval = 20 * time.Millisecond
	defer func() { directoryInterval = time.Minute }()
	stop := background(load(t, env), quiet())
	defer func() { _ = stop() }() // stops a server a failed wait left running before directoryInterval is reset; the wait's failure is the one reported
	dir, err := directory.Open(context.Background(), directoryURL)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	var entry directory.Entry
	pgtest.Eventually(t, 30*time.Second, func() bool {
		e, err := dir.Lookup(context.Background(), ten.ID)
		entry = e
		return err == nil
	}, "tenant %s not in the directory", ten.ID)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if entry.Cell != "test" {
		t.Errorf("directory entry cell: got %s, want test", entry.Cell)
	}
	if entry.APIURL != "https://test.api.example.com" {
		t.Errorf("directory entry api url: got %s, want https://test.api.example.com", entry.APIURL)
	}
	if entry.Status != directory.Active {
		t.Errorf("directory entry status: got %s, want %s", entry.Status, directory.Active)
	}
	cells, err := dir.ListCells(context.Background(), "eu-west")
	if err != nil {
		t.Fatal(err)
	}
	registered := false
	for _, c := range cells {
		registered = registered || c.Name == "test" && c.ControlURL == "http://test-admin.internal:9090" && c.Accepting
	}
	if !registered {
		t.Fatalf("the cell did not register itself: %+v", cells)
	}
	// A directory that cannot be opened stops the start.
	env["ERP_DIRECTORY_URL"] = "postgres://nobody@127.0.0.1:1/x?connect_timeout=1"
	if err := run(context.Background(), load(t, env), quiet(), nil); err == nil {
		t.Fatal("started without its directory")
	}
}

// wipeTrail removes a tenant's audit trail the way a destination cell
// would lack it: the erasure keeps it, an import brings it back.
func wipeTrail(t *testing.T, tenant uuid.UUID) {
	t.Helper()
	id := tenant.String()
	pgtest.Exec(t, `SELECT set_config('app.erasing', '`+id+`', true); DELETE FROM audit.verification WHERE tenant_id = '`+id+`'; DELETE FROM audit.anchor WHERE tenant_id = '`+id+`';
		DELETE FROM audit.audit_event WHERE tenant_id = '`+id+`'; DELETE FROM audit.chain_head WHERE tenant_id = '`+id+`'`)
}

// TestWorkflowEngine: with ERP_TEMPORAL_ADDRESS the server hosts a worker and
// dispatches queued operations to the engine; a payment proposal asked for
// through the API is built by the workflow. An engine that cannot be
// reached stops the start.
func TestWorkflowEngine(t *testing.T) {
	is := authntest.New(t)
	env := environment(t, is, "")
	env["ERP_TEMPORAL_ADDRESS"] = workflowtest.Server(t)
	env["ERP_TEMPORAL_TASK_QUEUE"] = "erp-test-" + uuid.NewString()[:8]
	env["ERP_TEMPORAL_CODEC_KEY"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("c", 32))) // payloads sealed on the way to the engine
	dispatchInterval = 50 * time.Millisecond
	defer func() { dispatchInterval = time.Second }()
	f := fixture.New(t, "payments.run.read", "payments.run.propose", "payments.run.release", "platform.operation.read")
	var bank uuid.UUID
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT id FROM payments.house_bank WHERE legal_entity_id = $1`, f.DE).Scan(&bank); err != nil {
		t.Fatal(err)
	}
	cfg := load(t, env)
	runCtx, cancel := context.WithCancel(context.Background())
	addrs := make(chan string, 1)
	done := make(chan error, 1)
	var logs pgtest.SyncBuffer // the server logs on its goroutines while the test reads
	go func() {
		done <- run(runCtx, cfg, slog.New(slog.NewTextHandler(&logs, nil)), func(api, _ net.Listener) { addrs <- api.Addr().String() })
	}()
	var addr string
	select {
	case addr = <-addrs:
	case err := <-done:
		t.Fatalf("stopped before serving: %v", err)
	}
	token := is.Token(t, f.Tenant.ID, f.Tenant.Human.Subject, nil)
	call := func(method, path, body string) (int, map[string]any) {
		req, _ := http.NewRequestWithContext(t.Context(), method, "http://"+addr+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		if method == "POST" {
			req.Header.Set("Idempotency-Key", uuid.NewString())
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		readJSON(t, resp, &m)
		return resp.StatusCode, m
	}
	code, op := call("POST", "/v1/payment-runs:propose", `{"legal_entity_id": "`+f.DE.String()+`", "house_bank_id": "`+bank.String()+`", "run_date": "2026-04-15", "due_by": "2026-04-15"}`)
	if code != 202 {
		t.Fatalf("propose: %d %v", code, op)
	}
	var final map[string]any
	pgtest.Eventually(t, 60*time.Second, func() bool {
		_, final = call("GET", "/v1/operations/"+op["id"].(string), "")
		return final["status"] == "succeeded" || final["status"] == "failed"
	}, "operation did not finish: %v\n%s", lazy(func() string { return fmt.Sprint(final) }), &logs)
	if final["status"] != "succeeded" {
		t.Fatalf("operation: %v\n%s", final, logs.String())
	}
	if _, run := call("GET", op["resource"].(string), ""); run["status"] != "proposed" {
		t.Fatalf("run: %v", run)
	}
	var workflowID *string
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT workflow_id FROM platform.operation WHERE id = $1`, uuid.MustParse(op["id"].(string))).Scan(&workflowID); err != nil {
		t.Fatalf("operation's workflow id: %v", err)
	}
	if workflowID == nil || !strings.Contains(*workflowID, "payment_run") {
		t.Fatalf("workflow id: %v", workflowID)
	}
	// The release, by the approver, is a workflow too: the run has nothing to pay here, which the API says at once.
	approver := is.Token(t, f.Tenant.ID, f.Approver.Subject, nil)
	req, _ := http.NewRequestWithContext(t.Context(), "POST", "http://"+addr+op["resource"].(string)+":release", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+approver)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", uuid.NewString())
	req.Header.Set("If-Match", "*")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var refused map[string]any
	readJSON(t, resp, &refused)
	if resp.StatusCode != 409 || refused["code"] != "payments.nothing_to_pay" {
		t.Fatalf("release of an empty run: %d %v", resp.StatusCode, refused)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "workflow engine connected") || !strings.Contains(logs.String(), "workflows started") {
		t.Fatalf("logs: %s", logs.String())
	}
	// An engine that cannot be reached stops the start (here without a codec key, which is warned about).
	env["ERP_TEMPORAL_ADDRESS"] = "127.0.0.1:1"
	delete(env, "ERP_TEMPORAL_CODEC_KEY")
	pgtest.WantErr(t, run(context.Background(), load(t, env), quiet(), nil), "connecting to")
}

// TestAFailedWorkflowWorkerStopsTheServer: the engine is reachable but the
// workflow worker cannot start (its namespace does not exist). A replica
// that kept serving would queue workflows its dispatcher hands to no
// worker of its own, and nothing would say so until shutdown; instead the
// server stops with the worker's error and a non-zero exit, its workers
// stopped before the pool closes.
func TestAFailedWorkflowWorkerStopsTheServer(t *testing.T) {
	is := authntest.New(t)
	env := environment(t, is, "")
	env["ERP_TEMPORAL_ADDRESS"] = workflowtest.Server(t)
	env["ERP_TEMPORAL_NAMESPACE"] = "no-such-namespace-" + uuid.NewString()[:8]
	env["ERP_TEMPORAL_TASK_QUEUE"] = "erp-test-" + uuid.NewString()[:8]
	env["ERP_TEMPORAL_CODEC_KEY"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("c", 32)))
	var s *server
	startWorkersHook = func(srv *server, runCtx context.Context) { s = srv; srv.startWorkers(runCtx) }
	defer func() { startWorkersHook = (*server).startWorkers }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs pgtest.SyncBuffer // the server logs on its goroutines while the test reads
	done := make(chan int, 1)
	go func() { done <- cli(ctx, nil, func(k string) string { return env[k] }, io.Discard, &logs, nil) }()
	var code int
	select {
	case code = <-done:
	case <-time.After(30 * time.Second):
		cancel()
		<-done
		t.Fatalf("got the server serving without its workflow worker, want it stopped\n%s", logs.String())
	}
	if code != 1 {
		t.Errorf("exit code: got %d, want 1", code)
	}
	if !strings.Contains(logs.String(), "workflow: worker on queue") {
		t.Errorf("logs: got %s, want the workflow worker's failure", logs.String())
	}
	if s == nil {
		t.Fatal("got no workers started, want them started before the failure")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, running := range s.running {
		if running {
			t.Errorf("%s: got still running after run returned, want stopped", name)
		}
	}
}

// status sends the request and returns the response's status, its body closed.
func status(t *testing.T, req *http.Request) int {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}

// readJSON decodes the response's JSON body into v and closes it; a body that is
// not the JSON the test asserts on fails the test here rather than as a confusing
// zero value later.
// request builds a request bound to the test's context; a body is sent as JSON.
func request(t *testing.T, method, url, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("request %s %s: %v", method, url, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func readJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode %s %s (%d): %v", resp.Request.Method, resp.Request.URL, resp.StatusCode, err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
}

// configFile writes the fixture configuration document to a file and returns its path.
func configFile(t *testing.T) string {
	t.Helper()
	doc, err := fixture.Document(t).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, doc, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// TestKeyringFailures: provision and rewrap report master keys that do not
// make a keyring instead of going on without one.
func TestKeyringFailures(t *testing.T) {
	cfg := config.Config{DatabaseURL: pgtest.URL(t), FieldKeys: []config.MasterKey{{ID: "k", Secret: []byte("short")}}}
	const want = "a master key needs an id and a secret"
	t.Run("provision", func(t *testing.T) {
		_, err := provision(context.Background(), cfg, provisioning.Request{}, configFile(t))
		pgtest.WantErr(t, err, want)
	})
	t.Run("rewrap", func(t *testing.T) {
		_, err := rewrap(context.Background(), cfg)
		pgtest.WantErr(t, err, want)
	})
	t.Run("rewrap without a master", func(t *testing.T) {
		_, err := rewrap(context.Background(), config.Config{DatabaseURL: pgtest.URL(t)})
		pgtest.WantErr(t, err, "no master key is configured")
	})
}

// TestAPanickingWorkerIsContained: a worker that panics does not crash the
// replica for every tenant. The panic is logged with its stack; a critical
// worker's panic stops the server with an error, as its failure would, and
// another worker's leaves the server serving.
func TestAPanickingWorkerIsContained(t *testing.T) {
	var logs pgtest.SyncBuffer
	s := &server{log: slog.New(slog.NewTextHandler(&logs, nil)), failed: make(chan struct{})}
	s.spawn("reporter", func() { panic("reporter boom") })
	s.wait()
	select {
	case <-s.failed:
		t.Fatalf("got the server failed by a worker that is not critical: %v", s.failure)
	default:
	}
	if !strings.Contains(logs.String(), "reporter boom") || !strings.Contains(logs.String(), "stack=") {
		t.Errorf("logs: got %s, want the panic with its stack", logs.String())
	}
	s.spawnCritical("workflow worker", func() error { panic("worker boom") })
	s.wait()
	select {
	case <-s.failed:
	default:
		t.Fatal("got the server serving after its critical worker panicked, want it stopped")
	}
	pgtest.WantErr(t, s.failure, "erp-server: workflow worker: panic: worker boom")
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, running := range s.running {
		if running {
			t.Errorf("worker %s: got running, want stopped", name)
		}
	}
}

// TestCellIssuers: the directory's rounds claim the cell's active identity
// providers' issuers for their tenants (ADR-0031).
func TestCellIssuers(t *testing.T) {
	ten := pgtest.NewTenant(t)
	issuer := "https://cell-" + uuid.NewString() + ".example.com"
	pgtest.Exec(t, `INSERT INTO identity.identity_provider (tenant_id, id, issuer, audience, created_by, updated_by) VALUES ($1, gen_random_uuid(), $2, 'erp', $1, $1)`, ten.ID, issuer)
	ctx := context.Background()
	tx, err := pgtest.Pool(t).Begin(ctx, database.Scope{Role: "erp_control", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	got, err := cellIssuers(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(got, func(is directory.Issuer) bool {
		return is.Issuer == issuer && is.Audience == "erp" && is.TenantID == ten.ID && is.ProviderID != uuid.Nil
	}) {
		t.Errorf("issuers: got %d without %s of the tenant, want it listed with its provider", len(got), issuer)
	}
}

// TestCellIssuersFailing: a failed read hands the round nothing, with the
// error.
func TestCellIssuersFailing(t *testing.T) {
	ctx := context.Background()
	tx, err := pgtest.Pool(t).Begin(ctx, database.Scope{Role: "erp_control", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	got, err := cellIssuers(ctx, &pgtest.FailingTx{Tx: tx, FailAt: 1})
	if !errors.Is(err, pgtest.ErrInjected) || got != nil {
		t.Errorf("got %v, %v, want nil and the injected failure", got, err)
	}
}

// TestProvisionWithAProvider: erp-server provision registers the tenant's
// identity provider, probed first, and binds the administrator to it.
func TestProvisionWithAProvider(t *testing.T) {
	idp := authntest.NewSeparate(t)
	env := environment(t, authntest.New(t), "")
	delete(env, "ERP_OIDC_ISSUERS")
	file := configFile(t)
	args := []string{"provision", "-name", "Acme", "-region", "eu-west", "-currency", "EUR", "-admin-subject", "ada", "-admin-name", "Ada",
		"-config", file, "-idp-issuer", idp.URL, "-idp-audience", authntest.Audience}
	var stdout, stderr bytes.Buffer
	if code := cli(context.Background(), args, func(k string) string { return env[k] }, &stdout, &stderr, nil); code != 0 {
		t.Fatalf("provision: %d %s", code, stderr.String())
	}
	var out struct {
		TenantID string  `json:"tenant_id"`
		Provider *string `json:"identity_provider_id"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || out.Provider == nil {
		t.Fatalf("output: %v %s", err, stdout.String())
	}
	var issuer string
	pgtest.Scan(t, `SELECT external_issuer FROM identity.principal WHERE tenant_id = $1 AND external_subject = 'ada'`, []any{out.TenantID}, &issuer)
	if issuer != idp.URL {
		t.Errorf("administrator's issuer: got %s, want %s", issuer, idp.URL)
	}
	if d, _ := idp.Fetches(); d != 1 {
		t.Errorf("discovery fetches: got %d, want the probe's 1", d)
	}
}

// TestRegisteredProviderEndToEnd runs the server with no deployment
// issuer for tenants (ADR-0031): an operator provisions a tenant with its
// identity provider, and that provider's token for the administrator signs
// in through the registry, the provider's keys, the session check and the
// unit of work; a token of an issuer the tenant did not register is
// refused.
func TestRegisteredProviderEndToEnd(t *testing.T) {
	ops, idp := authntest.New(t), authntest.NewSeparate(t)
	env := environment(t, ops, "")
	delete(env, "ERP_OIDC_ISSUERS")
	env["ERP_OPERATOR_ISSUERS"] = `[{"issuer": "` + ops.URL + `", "audience": "` + authntest.Audience + `"}]`
	ctx, cancel := context.WithCancel(context.Background())
	addrs, done := make(chan [2]string, 1), make(chan int, 1)
	var logs pgtest.SyncBuffer
	go func() {
		done <- cli(ctx, nil, func(k string) string { return env[k] }, io.Discard, &logs, func(api, admin net.Listener) {
			addrs <- [2]string{api.Addr().String(), admin.Addr().String()}
		})
	}()
	defer func() {
		cancel()
		if code := <-done; code != 0 {
			t.Errorf("exit %d\n%s", code, logs.String())
		}
	}()
	a := <-addrs
	doc, _ := json.Marshal(fixture.Document(t))
	body := `{"name": "Registered", "region": "eu-west", "group_currency": "EUR", "administrator": {"subject": "ada", "display_name": "Ada"},
		"identity_provider": {"issuer": "` + idp.URL + `", "audience": "` + authntest.Audience + `"}, "configuration": ` + string(doc) + `}`
	req := request(t, "POST", "http://"+a[1]+"/control/v1/tenants", body)
	req.Header.Set("Authorization", "Bearer "+ops.Token(t, uuid.Nil, "ops|e2e", map[string]any{"tenant_id": nil}))
	req.Header.Set("Idempotency-Key", "key-"+uuid.NewString())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var created map[string]any
	readJSON(t, resp, &created)
	if resp.StatusCode != 201 {
		t.Fatalf("provision: %d %v", resp.StatusCode, created)
	}
	tenant := created["id"].(string)
	get := func(token string) int {
		r := request(t, "GET", "http://"+a[0]+"/v1/roles", "")
		r.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close() // the status is what is asserted
		return resp.StatusCode
	}
	admin := idp.Token(t, uuid.Nil, "ada", map[string]any{"tenant_id": nil, "sid": "e2e", "auth_time": time.Now().Unix()})
	if code := get(admin); code != 200 {
		t.Fatalf("the administrator through the registered provider: got %d, want 200\n%s", code, logs.String())
	}
	var sessions int
	pgtest.Scan(t, `SELECT count(*) FROM identity.session WHERE tenant_id = $1`, []any{tenant}, &sessions)
	if sessions != 1 {
		t.Errorf("sessions: got %d, want the administrator's", sessions)
	}
	if code := get(ops.Token(t, uuid.MustParse(tenant), "ada", nil)); code != 401 {
		t.Errorf("an issuer the tenant did not register: got %d, want 401", code)
	}
}

// TestProvisionClaimsInTheDirectory: erp-server provision on a cell with a
// global directory claims the provider's issuer there before it commits,
// as the control plane does.
func TestProvisionClaimsInTheDirectory(t *testing.T) {
	idp := authntest.NewSeparate(t)
	env := environment(t, authntest.New(t), "")
	delete(env, "ERP_OIDC_ISSUERS")
	directoryURL := pgtest.EmptyDatabase(t)
	env["ERP_DIRECTORY_URL"], env["ERP_CELL_API_URL"], env["ERP_CELL_CONTROL_URL"], env["ERP_CELL_REGION"] = directoryURL, "https://test.api.example.com", "http://test-admin.internal:9090", "eu-west"
	args := []string{"provision", "-name", "Acme", "-region", "eu-west", "-currency", "EUR", "-admin-subject", "ada", "-admin-name", "Ada",
		"-config", configFile(t), "-idp-issuer", idp.URL, "-idp-audience", authntest.Audience}
	var stdout, stderr bytes.Buffer
	if code := cli(context.Background(), args, func(k string) string { return env[k] }, &stdout, &stderr, nil); code != 0 {
		t.Fatalf("provision: %d %s", code, stderr.String())
	}
	dir, err := directory.Open(context.Background(), directoryURL)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	claims, err := dir.LookupIssuer(context.Background(), idp.URL, []string{authntest.Audience})
	if err != nil || len(claims) != 1 || claims[0].Cell != "test" {
		t.Errorf("claims: got %v, %v, want the provider's, by this cell", claims, err)
	}
	env["ERP_DIRECTORY_URL"] = "postgres://nobody@127.0.0.1:1/x?connect_timeout=1"
	stderr.Reset()
	if code := cli(context.Background(), args, func(k string) string { return env[k] }, &stdout, &stderr, nil); code == 0 {
		t.Error("a directory that cannot be reached: got provisioned, want refused")
	}
}
