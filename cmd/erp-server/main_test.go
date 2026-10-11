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
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/natsbus"
	"github.com/mustie7iy/agentic-erp/internal/platform/natsbus/natstest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// environment is a valid configuration on the package database.
func environment(t *testing.T, is *authntest.Issuer, natsURL string) map[string]string {
	issuers, _ := json.Marshal([]any{is.Config()})
	return map[string]string{
		"DATABASE_URL":         pgtest.URL(t),
		"ERP_HTTP_ADDR":        "127.0.0.1:0",
		"ERP_ADMIN_ADDR":       "127.0.0.1:0",
		"ERP_CELL":             "test",
		"ERP_NATS_URL":         natsURL,
		"ERP_OIDC_ISSUERS":     string(issuers),
		"ERP_PAGE_TOKEN_KEY":   base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))),
		"ERP_FIELD_KEY":        base64.StdEncoding.EncodeToString([]byte(strings.Repeat("f", 32))),
		"ERP_MIGRATE_ON_START": "true",
		"ERP_SHUTDOWN_TIMEOUT": "5s",
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
	ten := pgtest.NewTenant(t, "platform.operation.read", "platform.operation.cancel")
	pgtest.Exec(t, `INSERT INTO platform.operation (tenant_id, id, kind, status, created_by, updated_by) VALUES ($1, gen_random_uuid(), 'payments.propose', 'running', $2, $2)`,
		ten.ID, ten.Human.ID)

	ctx, cancel := context.WithCancel(context.Background())
	addrs := make(chan [2]string, 1)
	done := make(chan int, 1)
	var logs bytes.Buffer
	go func() {
		done <- cli(ctx, func(k string) string { return env[k] }, &syncWriter{w: &logs}, func(api, admin net.Listener) {
			addrs <- [2]string{api.Addr().String(), admin.Addr().String()}
		})
	}()
	a := <-addrs
	get := func(u, token string) (*http.Response, map[string]any) {
		req, _ := http.NewRequest("GET", u, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		json.NewDecoder(resp.Body).Decode(&body)
		return resp, body
	}
	if resp, _ := get("http://"+a[1]+"/readyz", ""); resp.StatusCode != 200 {
		t.Fatalf("readyz %d", resp.StatusCode)
	}
	token := is.Token(t, ten.ID, ten.Human.Subject, nil)
	resp, page := get("http://"+a[0]+"/v1/operations", token)
	if resp.StatusCode != 200 || len(page["items"].([]any)) != 1 {
		t.Fatalf("list %d %v", resp.StatusCode, page)
	}
	op := page["items"].([]any)[0].(map[string]any)

	// Cancelling publishes OperationCompleted through the relay to JetStream.
	req, _ := http.NewRequest("POST", "http://"+a[0]+"/v1/operations/"+op["id"].(string)+":cancel", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", "key-server-test-1")
	req.Header.Set("If-Match", `"1"`)
	cresp, err := http.DefaultClient.Do(req)
	if err != nil || cresp.StatusCode != 200 {
		t.Fatal(cresp.StatusCode, err)
	}
	cresp.Body.Close()
	nc, _ := nats.Connect(natsURL)
	defer nc.Close()
	js, _ := jetstream.New(nc)
	stream, _ := js.Stream(context.Background(), natsbus.StreamName("test", "platform"))
	subject := fmt.Sprintf("erp.test.platform.%s.operation_completed", ten.ID)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if m, err := stream.GetLastMsgForSubject(context.Background(), subject); err == nil {
			if m.Header.Get("ce-subject") != op["id"] {
				t.Fatalf("event subject %s", m.Header.Get("ce-subject"))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("event not relayed")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d\n%s", code, logs.String())
	}
}

type syncWriter struct{ w io.Writer }

func (s *syncWriter) Write(p []byte) (int, error) { return s.w.Write(p) }

func TestReadiness(t *testing.T) {
	ctx := context.Background()
	if err := ready(pgtest.Pool(t))(ctx); err != nil {
		t.Fatal(err)
	}
	emptyURL := emptyDatabase(t)
	empty, err := database.Open(ctx, emptyURL, database.DefaultSettings)
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	if err := ready(empty)(ctx); err == nil {
		t.Fatal("database without migrations reported ready")
	}
	// A schema older than the binary is not ready.
	c, _ := pgx.Connect(ctx, emptyURL)
	defer c.Close(ctx)
	c.Exec(ctx, "CREATE TABLE public.erp_schema_migration (version integer PRIMARY KEY, name text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())")
	c.Exec(ctx, "INSERT INTO public.erp_schema_migration VALUES (1, '000001_foundation', now())")
	if err := ready(empty)(ctx); err == nil || !strings.Contains(err.Error(), "schema version 1") {
		t.Fatalf("old schema: %v", err)
	}
}

func TestStartupFailures(t *testing.T) {
	is := authntest.New(t)
	ctx := context.Background()
	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()
	cases := map[string]func(env map[string]string, cfg *config.Config){
		"otlp endpoint": func(_ map[string]string, c *config.Config) { c.OTLPEndpoint = "not a url" },
		"database": func(_ map[string]string, c *config.Config) {
			c.DatabaseURL = "postgres://nobody@127.0.0.1:1/x?connect_timeout=1"
		},
		"issuer":            func(_ map[string]string, c *config.Config) { c.Issuers[0].URL = "http://127.0.0.1:1" },
		"page key":          func(_ map[string]string, c *config.Config) { c.PageTokenKey = []byte("short") },
		"field key":         func(_ map[string]string, c *config.Config) { c.FieldKey = []byte("short") },
		"nats":              func(_ map[string]string, c *config.Config) { c.NATSURL = "nats://127.0.0.1:1" },
		"jetstream missing": func(_ map[string]string, c *config.Config) { c.NATSURL = natstest.Plain(t) },
		"api port":          func(_ map[string]string, c *config.Config) { c.HTTPAddr = busy.Addr().String() },
		"admin port":        func(_ map[string]string, c *config.Config) { c.AdminAddr = busy.Addr().String() },
		"migrations":        func(_ map[string]string, c *config.Config) { c.DatabaseURL = limitedUser(t) },
	}
	for name, mutate := range cases {
		env := environment(t, is, "")
		cfg := load(t, env)
		mutate(env, &cfg)
		if err := run(ctx, cfg, quiet(), nil); err == nil {
			t.Errorf("%s: started", name)
		}
	}
	// A meter provider that cannot create instruments fails the API.
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(failingProvider{})
	defer otel.SetMeterProvider(prev)
	if err := run(ctx, load(t, environment(t, is, "")), quiet(), nil); err == nil {
		t.Error("broken meter provider: started")
	}
}

func TestAListenerFailureStopsTheServer(t *testing.T) {
	is := authntest.New(t)
	cfg := load(t, environment(t, is, ""))
	err := run(context.Background(), cfg, quiet(), func(api, _ net.Listener) { api.Close() })
	if err == nil {
		t.Fatal("closed listener did not stop the server")
	}
}

func TestExitCodes(t *testing.T) {
	var stderr bytes.Buffer
	if code := cli(context.Background(), func(string) string { return "" }, &stderr, nil); code != 2 || !strings.Contains(stderr.String(), "DATABASE_URL") {
		t.Fatalf("bad configuration: %d %s", code, stderr.String())
	}
	is := authntest.New(t)
	env := environment(t, is, "")
	env["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] = "not a url"
	stderr.Reset()
	if code := cli(context.Background(), func(k string) string { return env[k] }, &stderr, nil); code != 1 {
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

func emptyDatabase(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	admin := pgtest.Admin(t)
	name := fmt.Sprintf("erp_empty_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, _ := pgx.Connect(ctx, pgtest.URL(t))
		c.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		c.Close(ctx)
	})
	u, _ := url.Parse(pgtest.URL(t))
	u.Path = "/" + name
	return u.String()
}

// limitedUser is a login that may connect but not create anything.
func limitedUser(t *testing.T) string {
	t.Helper()
	pgtest.Exec(t, `DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'erp_test_limited') THEN
		CREATE ROLE erp_test_limited LOGIN PASSWORD 'limited'; END IF; END $$`)
	u, _ := url.Parse(emptyDatabase(t))
	u.User = url.UserPassword("erp_test_limited", "limited")
	return u.String()
}

type failingMeter struct{ noop.Meter }

func (failingMeter) Int64Counter(string, ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	return nil, errors.New("no counters")
}

type failingProvider struct{ noop.MeterProvider }

func (failingProvider) Meter(string, ...metric.MeterOption) metric.Meter { return failingMeter{} }
