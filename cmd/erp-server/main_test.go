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
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore/blobstoretest"
	"github.com/mustie7iy/agentic-erp/internal/platform/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
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
	var logs bytes.Buffer
	go func() {
		done <- cli(ctx, nil, func(k string) string { return env[k] }, io.Discard, &syncWriter{w: &logs}, func(api, admin net.Listener) {
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
	// The workers' backlogs are read on each scrape of the admin metrics (a
	// read that outlasts its timeout on a loaded machine is left out of that
	// scrape, so a few scrapes are allowed).
	var scraped []byte
	for attempt := 0; attempt < 5; attempt++ {
		mresp, err := http.Get("http://" + a[1] + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		scraped, _ = io.ReadAll(mresp.Body)
		mresp.Body.Close()
		if strings.Contains(string(scraped), "erp_outbox_backlog ") && strings.Contains(string(scraped), "erp_projection_backlog 0") &&
			strings.Contains(string(scraped), "erp_payment_proposals_queued 0") && strings.Contains(string(scraped), "erp_simulations_queued 0") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, want := range []string{"erp_outbox_backlog ", "erp_projection_backlog 0", "erp_payment_proposals_queued 0", "erp_simulations_queued 0"} {
		if !strings.Contains(string(scraped), want) {
			t.Errorf("metrics lack %q:\n%s", want, scraped)
		}
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
	// A partition horizon that got too short is not ready either; maintenance restores it.
	shortURL := emptyDatabase(t)
	short, err := database.Open(ctx, shortURL, database.DefaultSettings)
	if err != nil {
		t.Fatal(err)
	}
	defer short.Close()
	if err := migrate(ctx, short, quiet()); err != nil {
		t.Fatal(err)
	}
	sc, _ := pgx.Connect(ctx, shortURL)
	defer sc.Close(ctx)
	if _, err := sc.Exec(ctx, `DO $$ DECLARE r record; BEGIN FOR r IN SELECT relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'audit' AND relname ~ '^audit_event_[0-9]{6}$' AND substr(relname, 13) > to_char(now(), 'YYYYMM')
		LOOP EXECUTE format('DROP TABLE audit.%I', r.relname); END LOOP; END $$`); err != nil {
		t.Fatal(err)
	}
	if err := ready(short)(ctx); err == nil || !strings.Contains(err.Error(), "partition horizon") {
		t.Fatalf("short horizon: %v", err)
	}
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
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	runs := 0
	done := make(chan struct{})
	go func() {
		maintain(ctx, time.Millisecond, func(context.Context) error {
			runs++
			if runs == 3 {
				cancel()
			}
			return errors.New("not now")
		}, slog.New(slog.NewTextHandler(&syncWriter{w: &logs}, nil)))
		close(done)
	}()
	<-done
	if runs != 3 || strings.Count(logs.String(), "partition maintenance failed") != 2 {
		t.Fatalf("%d runs\n%s", runs, logs.String())
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
		"issuer": func(_ map[string]string, c *config.Config) { c.Issuers[0].URL = "http://127.0.0.1:1" },
		"operator issuer": func(_ map[string]string, c *config.Config) {
			c.OperatorIssuers = []authn.Issuer{{URL: "http://127.0.0.1:1", Audience: "erp-api"}}
		},
		"page key":          func(_ map[string]string, c *config.Config) { c.PageTokenKey = []byte("short") },
		"field key":         func(_ map[string]string, c *config.Config) { c.FieldKey = []byte("short") },
		"nats":              func(_ map[string]string, c *config.Config) { c.NATSURL = "nats://127.0.0.1:1" },
		"jetstream missing": func(_ map[string]string, c *config.Config) { c.NATSURL = natstest.Plain(t) },
		"api port":          func(_ map[string]string, c *config.Config) { c.HTTPAddr = busy.Addr().String() },
		"admin port":        func(_ map[string]string, c *config.Config) { c.AdminAddr = busy.Addr().String() },
		"migrations":        func(_ map[string]string, c *config.Config) { c.DatabaseURL = limitedUser(t) },
		"partitions":        func(_ map[string]string, c *config.Config) { c.DatabaseURL, c.MigrateOnStart = emptyDatabase(t), false },
	}
	for name, mutate := range cases {
		env := environment(t, is, "")
		cfg := load(t, env)
		mutate(env, &cfg)
		if err := run(ctx, cfg, quiet(), nil); err == nil {
			t.Errorf("%s: started", name)
		}
	}
	// A meter provider that cannot create instruments fails the API. (The
	// global delegate cannot be set back, so a no-op provider takes its place.)
	otel.SetMeterProvider(failingProvider{})
	defer otel.SetMeterProvider(noop.NewMeterProvider())
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
	if code := cli(context.Background(), nil, func(string) string { return "" }, io.Discard, &stderr, nil); code != 2 || !strings.Contains(stderr.String(), "DATABASE_URL") {
		t.Fatalf("bad configuration: %d %s", code, stderr.String())
	}
	is := authntest.New(t)
	env := environment(t, is, "")
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

// TestProvisionAndDevelopmentLogin is the operator's path to a usable
// tenant: erp-server provision creates it, the development issuer mints its
// administrator a token, and the administrator works through the API.
func TestProvisionAndDevelopmentLogin(t *testing.T) {
	anchorInterval = 50 * time.Millisecond
	defer func() { anchorInterval = time.Minute }()
	env := environment(t, authntest.New(t), "")
	delete(env, "ERP_OIDC_ISSUERS")
	env["ERP_DEV_ISSUER"] = "true"
	getenv := func(k string) string { return env[k] }
	doc, _ := fixture.Document(t).Marshal()
	file := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(file, doc, 0o644)
	var stdout, stderr bytes.Buffer
	args := []string{"provision", "-name", "Acme", "-region", "eu-west", "-currency", "EUR", "-admin-subject", "idp|ada", "-admin-name", "Ada", "-admin-email", "ada@example.com", "-config", file}
	if code := cli(context.Background(), args, getenv, &stdout, &stderr, nil); code != 0 {
		t.Fatalf("provision: %d %s", code, stderr.String())
	}
	var created struct {
		TenantID        string `json:"tenant_id"`
		AdministratorID string `json:"administrator_id"`
		Version         int    `json:"configuration_version"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &created); err != nil || created.Version != 1 {
		t.Fatalf("%v %s", err, stdout.String())
	}
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
		stderr.Reset()
		if code := cli(context.Background(), c.args, getenv, io.Discard, &stderr, nil); code != c.code || !strings.Contains(stderr.String(), c.want) {
			t.Errorf("%s: %d %s", name, code, stderr.String())
		}
	}
	// Without a reachable database provisioning fails after the configuration was accepted.
	down := map[string]string{}
	for k, v := range env {
		down[k] = v
	}
	down["DATABASE_URL"] = "postgres://nobody@127.0.0.1:1/x?connect_timeout=1"
	if code := cli(context.Background(), args, func(k string) string { return down[k] }, io.Discard, &stderr, nil); code != 1 {
		t.Fatalf("database down: %d", code)
	}

	// The server with the development issuer: a minted token works on the API.
	ctx, cancel := context.WithCancel(context.Background())
	addrs := make(chan [2]string, 1)
	done := make(chan int, 1)
	var logs bytes.Buffer
	go func() {
		done <- cli(ctx, nil, getenv, io.Discard, &syncWriter{w: &logs}, func(api, admin net.Listener) {
			addrs <- [2]string{api.Addr().String(), admin.Addr().String()}
		})
	}()
	var a [2]string
	select {
	case a = <-addrs:
	case code := <-done:
		t.Fatalf("the server stopped with %d before serving:\n%s", code, logs.String())
	}
	mint, err := http.Post("http://"+a[1]+"/dev/token", "application/json", strings.NewReader(`{"tenant_id": "`+created.TenantID+`", "subject": "idp|ada"}`))
	if err != nil {
		t.Fatal(err)
	}
	var minted struct{ Token string }
	json.NewDecoder(mint.Body).Decode(&minted)
	mint.Body.Close()
	if mint.StatusCode != 200 || minted.Token == "" {
		t.Fatalf("mint: %d", mint.StatusCode)
	}
	req, _ := http.NewRequest("GET", "http://"+a[0]+"/v1/config-versions?is_active=true", nil)
	req.Header.Set("Authorization", "Bearer "+minted.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var page map[string]any
	json.NewDecoder(resp.Body).Decode(&page)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(page["items"].([]any)) != 1 {
		t.Fatalf("as the administrator: %d %v", resp.StatusCode, page)
	}
	// An operator token (no tenant) works on the control plane and nowhere else: the tenant is
	// suspended, refused at the API's door, and resumed.
	mint, err = http.Post("http://"+a[1]+"/dev/token", "application/json", strings.NewReader(`{"subject": "ops|root"}`))
	if err != nil {
		t.Fatal(err)
	}
	var operator struct{ Token string }
	json.NewDecoder(mint.Body).Decode(&operator)
	mint.Body.Close()
	control := func(method, path, ifMatch string) (int, map[string]any) {
		req, _ := http.NewRequest(method, "http://"+a[1]+"/control/v1"+path, nil)
		req.Header.Set("Authorization", "Bearer "+operator.Token)
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	if code, m := control("GET", "/tenants/"+created.TenantID, ""); code != 200 || m["status"] != "active" {
		t.Fatalf("control get: %d %v", code, m)
	}
	req, _ = http.NewRequest("GET", "http://"+a[0]+"/v1/roles", nil)
	req.Header.Set("Authorization", "Bearer "+operator.Token)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 401 {
		t.Fatalf("operator on the public API: %v %v", resp, err)
	}
	if code, m := control("POST", "/tenants/"+created.TenantID+":suspend", `"1"`); code != 200 || m["status"] != "suspended" {
		t.Fatalf("suspend: %d %v", code, m)
	}
	req, _ = http.NewRequest("GET", "http://"+a[0]+"/v1/roles", nil)
	req.Header.Set("Authorization", "Bearer "+minted.Token)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 403 {
		t.Fatalf("suspended tenant on the public API: %v %v", resp, err)
	}
	if code, m := control("POST", "/tenants/"+created.TenantID+":resume", `"2"`); code != 200 || m["status"] != "active" {
		t.Fatalf("resume: %d %v", code, m)
	}
	// The anchoring worker wrote the tenant's chain head to the object store.
	conn, err := pgx.Connect(ctx, env["DATABASE_URL"])
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var anchored int
	for deadline := time.Now().Add(10 * time.Second); anchored == 0 && time.Now().Before(deadline); {
		conn.QueryRow(ctx, `SELECT count(*) FROM audit.anchor WHERE tenant_id = $1`, created.TenantID).Scan(&anchored)
		time.Sleep(20 * time.Millisecond)
	}
	if anchored == 0 {
		t.Fatal("no anchor written")
	}
	if objects, _ := filepath.Glob(filepath.Join(env["ERP_OBJECT_DIR"], "tenants", created.TenantID, "audit", "anchors", "*.json")); len(objects) == 0 {
		t.Fatal("no anchor object")
	}
	cancel()
	if code := <-done; code != 0 || !strings.Contains(logs.String(), "ERP_DEV_ISSUER") || !strings.Contains(logs.String(), "control plane served") ||
		!strings.Contains(logs.String(), "suspendTenant") {
		t.Fatalf("exit %d\n%s", code, logs.String())
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
		var logs bytes.Buffer
		started := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- run(runCtx, cfg, slog.New(slog.NewTextHandler(&syncWriter{w: &logs}, nil)), func(net.Listener, net.Listener) { close(started) })
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
	if logs := serve(env); !strings.Contains(logs, "no object storage configured") {
		t.Fatalf("logs: %s", logs)
	}
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
	cfg := load(t, env)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- run(runCtx, cfg, quiet(), nil) }()
	key := "tenants/" + ten.ID.String() + "/audit/anchors/000000000001.json"
	for deadline := time.Now().Add(10 * time.Second); ; {
		if _, ok := s3.Objects[key]; ok || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if o, ok := s3.Objects[key]; !ok || o.LockMode != "COMPLIANCE" || o.ContentType != "application/json" {
		t.Fatalf("anchor in S3: %+v (%d objects)", o, len(s3.Objects))
	}
}
