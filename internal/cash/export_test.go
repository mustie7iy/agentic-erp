package cash_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/sftpclient/sftptest"
)

// exportEnv is Maple Inc's snapshot setting with a treasury system's SFTP
// server and the treasury channels to it (ADR-0051 §14, T53h).
type exportEnv struct {
	drillEnv
	server             *sftptest.Server
	jsonl, csv, others uuid.UUID // channels of 3000 in each format, and one of another company
	clock              *exportClock
}

// exportClock is the exporter's clock, which a test moves.
type exportClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *exportClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *exportClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// treasuryKey is the environment variable the channels' secret_ref names.
const treasuryKey = "CASH_TREASURY_KEY"

func setupExport(t *testing.T) exportEnv {
	t.Helper()
	e := exportEnv{drillEnv: setupSnapshot(t), clock: &exportClock{at: readAt.Add(time.Minute)}}
	e.Tenant.Grant(t, "cash.export.read", e.Tenant.Human)
	e.Tenant.Grant(t, "cash.export.send", e.Tenant.Human)
	key, client := sftptest.PrivateKey(t)
	t.Setenv(treasuryKey, string(key))
	e.server = sftptest.NewServer(t, client.PublicKey(), func(*sftptest.Server) {})
	e.server.Mkdir(t, "/cash")
	e.jsonl = e.channel(t, "Treasury A", "active", `"format": "jsonl", "companies": ["3000"]`)
	e.csv = e.channel(t, "Treasury B", "active", `"format": "csv", "companies": ["4000", "3000"]`)
	e.others = e.channel(t, "Treasury of 4000", "active", `"format": "csv", "companies": ["4000"]`)
	// The exporter of every test sends every tenant's exports: none of
	// this one's is left for the next test to send to a server gone.
	t.Cleanup(func() {
		pgtest.Exec(t, `UPDATE cash.export SET status = 'failed', next_attempt_at = NULL, version = version + 1 WHERE tenant_id = $1 AND status = 'pending'`,
			e.Tenant.ID)
	})
	return e
}

// channel writes a treasury_export connector to the test server.
func (e exportEnv) channel(t *testing.T, name, status, fields string) uuid.UUID {
	t.Helper()
	host, port, err := net.SplitHostPort(e.server.Addr)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.Must(uuid.NewV7())
	settings := fmt.Sprintf(`{"host": %q, "port": %s, "user": "erp", "host_key": %q, "directory": "/cash", %s}`, host, port,
		strings.TrimSpace(e.server.HostKey()), fields)
	pgtest.Exec(t, `INSERT INTO integration.connector (tenant_id, id, kind, name, settings, secret_ref, status, created_by, updated_by)
		VALUES ($1, $2, 'treasury_export', $3, $4, 'env:`+treasuryKey+`', $5, $6, $6)`, e.Tenant.ID, id, name, settings, status, e.Tenant.Human.ID)
	return id
}

func (e exportEnv) exporter() *cash.Exporter {
	return &cash.Exporter{Endpoints: app.Cash(app.Config{Now: func() time.Time { return readAt }}), Pool: pgtest.Pool(e.T), AppRole: "erp_app",
		WorkerRole: "erp_worker", Now: e.clock.now}
}

// round runs the exporter once and checks how many exports it handled.
func (e exportEnv) round(t *testing.T, want int) {
	t.Helper()
	n, err := e.exporter().RunOnce(context.Background())
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	if n != want {
		t.Fatalf("round: got %d exports handled, want %d", n, want)
	}
}

// exports are the snapshot's exports, newest first.
func (e exportEnv) exports(t *testing.T, snapshot string) []map[string]any {
	t.Helper()
	p, _ := e.Must(200, "GET", "/v1/cash-exports?cash_snapshot_id="+snapshot, "", "")
	var out []map[string]any
	for _, it := range p["items"].([]any) {
		out = append(out, it.(map[string]any))
	}
	return out
}

// byChannel is the export to the channel among them.
func byChannel(t *testing.T, xs []map[string]any, ch uuid.UUID) map[string]any {
	t.Helper()
	for _, x := range xs {
		if x["connector_id"] == ch.String() {
			return x
		}
	}
	t.Fatalf("no export to %s among %v", ch, xs)
	return nil
}

var fileName = regexp.MustCompile(`^cash-3000-2026-04-17-[0-9a-f]{64}\.(jsonl|csv)$`)

// rowsOf renders a JSON lines file's rows as "currency granularity start
// [..end] bank/last4 source/certainty item amount xcount" lines.
func rowsOf(t *testing.T, content []byte) []string {
	t.Helper()
	var out []string
	dec := json.NewDecoder(bytes.NewReader(content))
	for dec.More() {
		var r map[string]any
		if err := dec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		period := fmt.Sprint(r["period_start"])
		if r["granularity"] == "bucket" {
			period += ".." + fmt.Sprint(r["period_end"])
		}
		out = append(out, fmt.Sprintf("%s %s %s %v/%v %s/%s %v %s x%v", r["currency"], r["granularity"], period, r["house_bank_code"], r["account_last4"],
			r["source"], r["certainty"], r["liquidity_item"], r["amount"], r["flow_count"]))
	}
	return out
}

// TestCashExport: a snapshot taken is queued for export to every active
// treasury channel naming its company, each sent in its format by the
// exporter, named by company, date and content hash, audited and
// published; asked for again, the same file under the same name is
// recognised as a resend (ADR-0051 §14, T53h).
func TestCashExport(t *testing.T) {
	e := setupExport(t)
	e.channel(t, "Off", "disabled", `"format": "jsonl", "companies": ["3000"]`)
	s := e.take(t, "Board meeting April")
	sid := s["id"].(string)
	queued := e.exports(t, sid)
	want(t, "queued", len(queued), 2)
	for _, ch := range []uuid.UUID{e.jsonl, e.csv} {
		x := byChannel(t, queued, ch)
		want(t, "queued pending", fmt.Sprint(x["status"], " ", x["attempts"], " ", x["file_name"], " ", x["company_code"], " ", x["snapshot_date"]),
			"pending 0 <nil> 3000 2026-04-17")
	}
	want(t, "the channel named", byChannel(t, queued, e.jsonl)["connector_name"], "Treasury A")
	e.round(t, 2)
	delivered := e.exports(t, sid)
	first := byChannel(t, delivered, e.jsonl)
	want(t, "delivered", fmt.Sprint(first["status"], " ", first["attempts"], " ", first["resend"], " ", first["format"]), "delivered 1 false jsonl")
	name := first["file_name"].(string)
	if !fileName.MatchString(name) {
		t.Fatalf("file name: got %s, want cash-3000-2026-04-17-<sha256>.jsonl", name)
	}
	content, err := e.server.ReadFile("/cash/"+name, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	want(t, "the hash in its name and on the export", fmt.Sprint(strings.Contains(name, hex.EncodeToString(sum[:])), " ", first["sha256"]),
		fmt.Sprint(true, " ", hex.EncodeToString(sum[:])))
	want(t, "rows", rowsOf(t, content), []string{
		"CAD day 2026-04-17 MAIN/0001 payment_run/confirmed OP_SUPPLIERS -800.00 x1",
		"CAD day 2026-04-22 MAIN/0001 payment_run/confirmed OP_SUPPLIERS -300.00 x1",
		"CAD day 2026-04-23 MAIN/0001 memo/expected REC_CUSTOMERS 2000.00 x1",
		"CAD day 2026-04-24 <nil>/<nil> memo/expected REC_CUSTOMERS 100.00 x1",
		"CAD day 2026-04-28 <nil>/<nil> down_payment/expected OP_SUPPLIERS -250.00 x1",
		"CAD day 2026-04-30 MAIN/0001 payable/expected OP_SUPPLIERS -900.00 x1",
		"CAD day 2026-05-12 <nil>/<nil> memo/planned OP_SUPPLIERS -500.00 x1",
		"CAD bucket 2026-05-18..2026-05-21 MAIN/0001 payable/expected OP_SUPPLIERS -200.00 x1",
		"CAD bucket 2026-06-05..2026-06-11 MAIN/0001 payable/expected OP_SUPPLIERS -600.00 x1",
		"CAD bucket 2026-07-17..2026-08-16 <nil>/<nil> memo/expected REC_CUSTOMERS 100.00 x1",
		"CAD bucket 2026-10-17..2026-11-16 <nil>/<nil> memo/expected REC_CUSTOMERS 100.00 x1",
		"CAD bucket 2027-01-17..2027-02-16 <nil>/<nil> memo/expected REC_CUSTOMERS 100.00 x1",
		"USD day 2026-04-17 USD/0001 in_transit/confirmed OP_SUPPLIERS -300.00 x1",
		"USD day 2026-04-22 <nil>/<nil> memo/planned OP_SUPPLIERS -0.02 x1",
		"USD day 2026-04-24 <nil>/<nil> memo/planned OP_SUPPLIERS -0.02 x1",
	})
	csv := byChannel(t, delivered, e.csv)
	csvContent, err := e.server.ReadFile("/cash/"+csv["file_name"].(string), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	want(t, "the CSV: a header and the same rows", bytes.Count(csvContent, []byte("\r\n")), 16)
	events := apitest.Published(t, e.Tenant, outbox.Event{Context: "cash", Name: "cash_export_delivered"})
	want(t, "published", len(events), 2)

	// Asked for again: the same file under the same name, a resend.
	again, _ := e.Must(201, "POST", "/v1/cash-snapshots/"+sid+"/exports", `{"connector_id": "`+e.jsonl.String()+`"}`, "")
	want(t, "asked for", fmt.Sprint(again["status"], " ", again["connector_name"], " ", again["cash_snapshot_id"]), "pending Treasury A "+sid)
	e.round(t, 1)
	resent, _ := e.Must(200, "GET", "/v1/cash-exports/"+again["id"].(string), "", "")
	want(t, "resent", fmt.Sprint(resent["status"], " ", resent["resend"], " ", resent["file_name"] == name, " ", resent["sha256"] == first["sha256"]),
		"delivered true true true")
	events = apitest.Published(t, e.Tenant, outbox.Event{Context: "cash", Name: "cash_export_delivered"})
	want(t, "published as a resend", fmt.Sprint(len(events), " ", events[2]["resend"], " ", events[2]["file_name"]), fmt.Sprint(3, " ", true, " ", name))
	var requested, deliveredActions int
	var foundThere bool
	pgtest.Scan(t, `SELECT count(*) FILTER (WHERE action = 'cash.export.request'), count(*) FILTER (WHERE action = 'cash.export.deliver'),
			bool_or((detail->>'found_there')::boolean) FILTER (WHERE action = 'cash.export.deliver' AND object_id = $2)
		FROM audit.audit_event WHERE tenant_id = $1`, []any{e.Tenant.ID, again["id"]}, &requested, &deliveredActions, &foundThere)
	want(t, "audited", fmt.Sprint(requested, " ", deliveredActions, " ", foundThere), "3 3 true")
	l, _ := e.Must(200, "GET", "/v1/cash-exports?status=delivered&legal_entity_id="+e.le.String()+"&page_size=2", "", "")
	want(t, "listed newest first, paged", fmt.Sprint(len(l["items"].([]any)), " ", l["items"].([]any)[0].(map[string]any)["id"], " ", l["next_page_token"] != nil),
		fmt.Sprint(2, " ", again["id"], " ", true))
	l, _ = e.Must(200, "GET", "/v1/cash-exports?connector_id="+e.csv.String(), "", "")
	want(t, "listed by channel", fmt.Sprint(len(l["items"].([]any)), " ", l["items"].([]any)[0].(map[string]any)["connector_name"]), "1 Treasury B")
	want(t, "the event names the channel", events[2]["connector_name"], "Treasury A")
}

// TestCashExportChannelChanged: an export waiting for its next attempt is
// refused when its channel no longer exports its company, or no longer
// takes its format, rather than sent where it is no longer set up for.
func TestCashExportChannelChanged(t *testing.T) {
	e := setupExport(t)
	sid := e.take(t, "Board")["id"].(string)
	pgtest.Exec(t, `UPDATE integration.connector SET settings = jsonb_set(settings, '{companies}', '["4000"]'), version = version + 1 WHERE id = $1`, e.jsonl)
	pgtest.Exec(t, `UPDATE integration.connector SET settings = jsonb_set(settings, '{format}', '"jsonl"'), version = version + 1 WHERE id = $1`, e.csv)
	e.round(t, 2)
	for _, ch := range []uuid.UUID{e.jsonl, e.csv} {
		x := byChannel(t, e.exports(t, sid), ch)
		want(t, "refused", fmt.Sprint(x["status"], " ", x["attempts"], " ", x["last_error_code"]), "failed 1 cash.export_channel_invalid")
	}
	if _, err := e.server.ReadFile("/cash/"+fmt.Sprint(byChannel(t, e.exports(t, sid), e.jsonl)["file_name"]), 1<<20); err == nil {
		t.Error("file: got one sent, want none")
	}
}

// TestCashExportSameFileAtOnce: two exports of one snapshot to one
// channel, sent by two exporters at once, are delivered one after the
// other: exactly one of them is a resend.
func TestCashExportSameFileAtOnce(t *testing.T) {
	e := setupExport(t)
	sid := e.take(t, "Board")["id"].(string)
	pgtest.Exec(t, `UPDATE cash.export SET status = 'failed', next_attempt_at = NULL, version = version + 1 WHERE snapshot_id = $1 AND connector_id = $2`, sid, e.csv)
	e.Must(201, "POST", "/v1/cash-snapshots/"+sid+"/exports", `{"connector_id": "`+e.jsonl.String()+`"}`, "")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Go(func() { _, errs[i] = e.exporter().RunOnce(context.Background()) })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var resends []string
	for _, x := range e.exports(t, sid) {
		if x["connector_id"] == e.jsonl.String() {
			resends = append(resends, fmt.Sprint(x["status"], " ", x["resend"]))
		}
	}
	slices.Sort(resends)
	want(t, "one first delivery, one resend", resends, []string{"delivered false", "delivered true"})
}

// TestCashExportRefusals: an export is asked for only to an active
// treasury channel naming the snapshot's company, of a snapshot of a
// company the caller may act in; exports of others are not seen.
func TestCashExportRefusals(t *testing.T) {
	e := setupExport(t)
	sid := e.take(t, "Board")["id"].(string)
	off := e.channel(t, "Off", "disabled", `"format": "jsonl", "companies": ["3000"]`)
	var bank uuid.UUID
	pgtest.Scan(t, `SELECT connector_id FROM payments.house_bank WHERE id = $1`, []any{e.main}, &bank)
	body := func(id uuid.UUID) string { return `{"connector_id": "` + id.String() + `"}` }
	for name, ch := range map[string]uuid.UUID{"another company's": e.others, "disabled": off, "no connector": uuid.New()} {
		t.Run(name, func(t *testing.T) {
			e.Problem(422, "cash.export_channel_invalid", "POST", "/v1/cash-snapshots/"+sid+"/exports", body(ch), "")
		})
	}
	e.Problem(404, "request.not_found", "POST", "/v1/cash-snapshots/"+uuid.NewString()+"/exports", body(e.jsonl), "")
	other := e.Reader(t, "reader of 1000", e.US)
	e.Tenant.Grant(t, "cash.export.send", other)
	e.Tenant.Grant(t, "cash.export.read", other)
	e.As(other).Problem(404, "request.not_found", "POST", "/v1/cash-snapshots/"+sid+"/exports", body(e.jsonl), "")
	x := e.exports(t, sid)[0]
	e.As(other).Problem(404, "request.not_found", "GET", "/v1/cash-exports/"+x["id"].(string), "", "")
	p, _ := e.As(other).Must(200, "GET", "/v1/cash-exports", "", "")
	want(t, "another company's reader lists none", len(p["items"].([]any)), 0)
	e.Problem(404, "request.not_found", "GET", "/v1/cash-exports/"+uuid.NewString(), "", "")
	e.Problem(400, "request.invalid_page_token", "GET", "/v1/cash-exports?page_token=x", "", "")
	e.round(t, 2)
}

// TestCashExportRetries: a failed upload is tried again after its wait,
// up to six attempts, then fails; a channel that cannot be used fails at
// once; a claim whose outcome never came is claimed again, and the second
// upload, finding the file there, delivers it once (the run twice of a
// lost commit).
func TestCashExportRetries(t *testing.T) {
	e := setupExport(t)
	e.server.FailOpen = true
	sid := e.take(t, "Board")["id"].(string)
	e.round(t, 2)
	x := byChannel(t, e.exports(t, sid), e.jsonl)
	want(t, "after a failed attempt", fmt.Sprint(x["status"], " ", x["attempts"], " ", x["last_error_code"]), "pending 1 platform.unavailable")
	e.round(t, 0) // not due before its wait
	for attempt := 2; attempt <= 6; attempt++ {
		e.clock.add(5 * time.Hour)
		e.round(t, 2)
	}
	x = byChannel(t, e.exports(t, sid), e.jsonl)
	want(t, "after the sixth", fmt.Sprint(x["status"], " ", x["attempts"], " ", x["next_attempt_at"]), "failed 6 <nil>")

	e.server.FailOpen = false
	pgtest.Exec(t, `UPDATE integration.connector SET status = 'disabled', version = version + 1 WHERE id = $1`, e.csv)
	pending, _ := e.Must(201, "POST", "/v1/cash-snapshots/"+sid+"/exports", `{"connector_id": "`+e.jsonl.String()+`"}`, "")
	pgtest.Exec(t, `INSERT INTO cash.export (tenant_id, id, snapshot_id, legal_entity_id, connector_id, format, status, next_attempt_at, requested_by)
		VALUES ($1, $2, $3, $4, $5, 'csv', 'pending', $6, $7)`, e.Tenant.ID, uuid.Must(uuid.NewV7()), sid, e.le, e.csv, readAt, e.Tenant.Human.ID)
	lost := e.exporter()
	lost.Begin = loseCommit(t, 2) // the first export's claim commits, its outcome is lost
	if _, err := lost.RunOnce(context.Background()); err == nil {
		t.Fatal("got the lost commit unreported, want it")
	}
	x = mustExport(t, e, pending["id"].(string))
	want(t, "the outcome lost", fmt.Sprint(x["status"], " ", x["attempts"]), "pending 1")
	e.round(t, 1) // the disabled channel's
	e.clock.add(11 * time.Minute)
	e.round(t, 1)
	x = mustExport(t, e, pending["id"].(string))
	want(t, "claimed again, delivered once", fmt.Sprint(x["status"], " ", x["attempts"], " ", x["resend"]), "delivered 2 false")
	refused := byChannel(t, e.exports(t, sid), e.csv)
	want(t, "the disabled channel's", fmt.Sprint(refused["status"], " ", refused["attempts"], " ", refused["last_error_code"]), "failed 1 integration.connector_disabled")
	events := 0
	for _, ev := range apitest.Published(t, e.Tenant, outbox.Event{Context: "cash", Name: "cash_export_delivered"}) {
		if ev["cash_export_id"] == pending["id"] {
			events++
		}
	}
	want(t, "published once", events, 1)
}

// TestCashExportLastAttemptLapsing: a last attempt whose replica stops
// before recording every outcome is made again once, then fails, rather
// than claimed again at every lease; an outcome whose commit applied but
// was not acknowledged is recorded and published once (ADR-0012).
func TestCashExportLastAttemptLapsing(t *testing.T) {
	e := setupExport(t)
	sid := e.take(t, "Board")["id"].(string)
	pgtest.Exec(t, `UPDATE cash.export SET status = 'failed', next_attempt_at = NULL, version = version + 1 WHERE snapshot_id = $1 AND connector_id = $2`, sid, e.csv)
	x := byChannel(t, e.exports(t, sid), e.jsonl)["id"].(string)
	pgtest.Exec(t, `UPDATE cash.export SET attempts = 5, last_error = 'platform.unavailable', version = version + 1 WHERE id = $1`, x)
	claims := 0
	for ; claims < 4 && mustExport(t, e, x)["status"] == cash.ExportPending; claims++ {
		lost := e.exporter()
		lost.Begin = loseCommit(t, 2) // the claim commits, the outcome is lost: the replica stopped
		if _, err := lost.RunOnce(context.Background()); err != nil && !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("claim %d: got %v, want at most the lost outcome reported", claims+1, err)
		}
		e.clock.add(11 * time.Minute)
	}
	got := mustExport(t, e, x)
	want(t, "ended after the last attempt and its remake", fmt.Sprint(claims, " ", got["status"], " ", got["attempts"], " ", got["last_error_code"]),
		"3 failed 6 cash.export_outcome_unknown")
	var audited int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2 AND action = 'cash.export.fail'`, []any{e.Tenant.ID, x}, &audited)
	want(t, "the failure audited", audited, 1)

	// Asked for again, the outcome's commit applies but is not
	// acknowledged: delivered and published once, not claimed again.
	again, _ := e.Must(201, "POST", "/v1/cash-snapshots/"+sid+"/exports", `{"connector_id": "`+e.jsonl.String()+`"}`, "")
	w := e.exporter()
	begun := 0
	w.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pgtest.Pool(t).Begin(ctx, s)
		if err != nil || s.TenantID == uuid.Nil {
			return tx, err
		}
		if begun++; begun == 2 {
			return ackLostTx{tx}, nil
		}
		return tx, nil
	}
	if _, err := w.RunOnce(context.Background()); !errors.Is(err, pgtest.ErrInjected) {
		t.Fatalf("got %v, want the unacknowledged commit reported", err)
	}
	e.clock.add(11 * time.Minute)
	e.round(t, 0)
	delivered := mustExport(t, e, again["id"].(string))
	want(t, "delivered once", fmt.Sprint(delivered["status"], " ", delivered["attempts"]), "delivered 1")
	events := 0
	for _, ev := range apitest.Published(t, e.Tenant, outbox.Event{Context: "cash", Name: "cash_export_delivered"}) {
		if ev["cash_export_id"] == again["id"] {
			events++
		}
	}
	want(t, "published once", events, 1)
}

// ackLostTx commits, then reports the commit failed: a commit whose
// acknowledgement was lost.
type ackLostTx struct{ pgx.Tx }

func (a ackLostTx) Commit(ctx context.Context) error {
	if err := a.Tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return pgtest.ErrInjected
}

func mustExport(t *testing.T, e exportEnv, id string) map[string]any {
	t.Helper()
	x, _ := e.Must(200, "GET", "/v1/cash-exports/"+id, "", "")
	return x
}

// loseCommit begins transactions on the test pool, the nth of a tenant's
// failing its commit, as a commit whose acknowledgement is lost.
func loseCommit(t *testing.T, n int) database.BeginFunc {
	t.Helper()
	pool := pgtest.Pool(t)
	begun := 0
	return func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		if err != nil || s.TenantID == uuid.Nil {
			return tx, err
		}
		if begun++; begun == n {
			return &pgtest.FailingTx{Tx: tx, FailCommit: true}, nil
		}
		return tx, nil
	}
}
