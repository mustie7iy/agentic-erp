package nfr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/natsbus"
	"github.com/mustie7iy/agentic-erp/internal/platform/natsbus/natstest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/ratelimit"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
)

// Permissions are what the users of a load test hold, besides the approval
// permissions the fixture adds.
var Permissions = []string{"ledger.entry.post", "ledger.entry.read", "ledger.account.read", "procurement.requisition.create",
	"procurement.requisition.read", "audit.event.read", "audit.chain.verify", "payments.run.propose", "payments.run.read",
	"platform.operation.read"}

// Cell is the cell name of a stack's event subjects and streams.
const Cell = "nfr"

// Options shape a Stack.
type Options struct {
	Users       int                // virtual users, each its own principal; default 8
	Events      bool               // run the outbox relay to an embedded JetStream server
	Publisher   outbox.Publisher   // relay to this instead of the embedded server (Events must be set)
	NoProjector bool               // leave balances unprojected
	Limits      *ratelimit.Limiter // request quotas; nil applies none
	Timeout     time.Duration      // request deadline; 0 sets none
}

// Stack is the platform under test as erp-server runs it: the public API on
// a real listener over a configured tenant, the balance projector and the
// outbox relay, and the users of a load test. It is test support and is
// excluded from the coverage figure.
type Stack struct {
	T       testing.TB
	Env     fixture.Env
	Config  httpapi.Config
	Server  *httptest.Server
	Pool    *database.Pool
	Metrics *telemetry.Metrics
	NATS    *natstest.Restartable
	Bus     *natsbus.Bus
	Users   []*User
	Client  *http.Client
	// Accounts of company 2000 (EUR) the load posts to.
	Expense, Bank, Office uuid.UUID

	Projector *ledger.Projector
	Relay     *outbox.Relay

	cancel  context.CancelFunc
	workers sync.WaitGroup
	tokens  sync.Map // subject → token
	issued  sync.Map // subject → time the token was issued

	mu     sync.Mutex
	posted []Posted
	admin  *pgx.Conn // one superuser connection for the checks
}

// Posted is a journal entry a user posted: its idempotency key, which is
// also its reference, and the answer the user got.
type Posted struct {
	Key, ID string
	Status  int
	Err     error
}

// Start builds and starts a stack.
func Start(t testing.TB, o Options) *Stack {
	t.Helper()
	if o.Users <= 0 {
		o.Users = 8
	}
	s := &Stack{T: t, Metrics: telemetry.NewMetrics()}
	s.Env = fixture.New(t, Permissions...)
	s.Env.API = apitest.NewWith(t, func(c *httpapi.Config) {
		c.Limits, c.RequestTimeout, c.Metrics = o.Limits, o.Timeout, s.Metrics
		s.Config = *c
	})
	s.Pool = pgtest.Pool(t)
	s.admin = pgtest.Admin(t)
	s.Expense, s.Bank, s.Office = s.Env.Account(t, "2000", "520000"), s.Env.Account(t, "2000", "100000"), s.Env.Account(t, "2000", "510000")
	for i := range o.Users {
		s.Users = append(s.Users, &User{s: s, Actor: s.Env.Person(t, fmt.Sprintf("user%03d", i)), n: i})
	}
	s.Server = httptest.NewUnstartedServer(s.Env.API.Handler)
	s.Server.Config.ReadHeaderTimeout, s.Server.Config.IdleTimeout = 10*time.Second, 120*time.Second
	s.Server.Start()
	s.Client = &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{MaxIdleConns: 4 * o.Users, MaxIdleConnsPerHost: 4 * o.Users}}
	// Before the workers start: a start that fails half way (tb.Fatal) still
	// stops the server and whatever workers did start.
	t.Cleanup(s.Stop)
	s.startWorkers(t, o)
	return s
}

// startWorkers starts the stack's projector and, with Events, its outbox
// relay to NATS (a restartable test server unless the options bring a
// publisher); Stop stops them.
func (s *Stack) startWorkers(tb testing.TB, o Options) {
	tb.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	log := telemetry.NewLogger(io.Discard, 0)
	if !o.NoProjector {
		s.Projector = &ledger.Projector{Pool: s.Pool, Role: "erp_projector", Interval: 100 * time.Millisecond, Log: log}
		s.workers.Add(1)
		go func() { defer s.workers.Done(); s.Projector.Run(ctx) }()
	}
	if o.Events {
		publisher := o.Publisher
		if publisher == nil {
			s.NATS = natstest.NewRestartable(tb)
			bus, err := natsbus.Connect(s.NATS.URL)
			if err != nil {
				tb.Fatal(err)
			}
			s.Bus, publisher = bus, bus // set before the streams, so Stop closes it if they fail
			if err := ensureStreams(bus, ctx, Cell); err != nil {
				tb.Fatal(err)
			}
		}
		s.Relay = &outbox.Relay{Pool: s.Pool, Role: "erp_relay", Publisher: publisher, Cell: Cell, Interval: 100 * time.Millisecond, Log: log}
		s.workers.Add(1)
		go func() { defer s.workers.Done(); s.Relay.Run(ctx) }()
	}
}

// ensureStreams makes the stack's streams on its bus; tests replace it to
// fail the start.
var ensureStreams = (*natsbus.Bus).EnsureStreams

// Stop stops the server, then the workers; it also ends a stack whose start
// failed before every worker was running.
func (s *Stack) Stop() {
	s.Server.Close()
	if s.cancel != nil {
		s.cancel()
	}
	s.workers.Wait()
	if s.Bus != nil {
		s.Bus.Close()
		s.Bus = nil
	}
	s.admin.Close(context.Background())
}

// Token is a bearer token of the actor, refreshed every 30 minutes (the
// test issuer's tokens live an hour; a soak outlives them).
func (s *Stack) Token(a pgtest.Actor) string {
	if at, ok := s.issued.Load(a.Subject); ok && time.Since(at.(time.Time)) < 30*time.Minute {
		tok, _ := s.tokens.Load(a.Subject)
		return tok.(string)
	}
	tok := s.Env.API.Issuer.Token(s.T, s.Env.Tenant.ID, a.Subject, nil)
	s.tokens.Store(a.Subject, tok)
	s.issued.Store(a.Subject, time.Now())
	return tok
}

// Req is one API call over the wire.
type Req struct {
	Method, Path, Body string
	As                 pgtest.Actor
	IfMatch, Key       string // Key: Idempotency-Key; empty gives unsafe calls a fresh one
}

// Result is what came back.
type Result struct {
	Status int
	Body   []byte
	ETag   string
	Took   time.Duration
	Err    error
}

// OK reports a 2xx answer.
func (r Result) OK() bool { return r.Err == nil && r.Status >= 200 && r.Status < 300 }

// JSON decodes the body; it is nil when the body is not an object.
func (r Result) JSON() map[string]any {
	var m map[string]any
	_ = json.Unmarshal(r.Body, &m)
	return m
}

// Call performs a request and measures it.
func (s *Stack) Call(ctx context.Context, r Req) Result {
	req, err := http.NewRequestWithContext(ctx, r.Method, s.Server.URL+r.Path, strings.NewReader(r.Body))
	if err != nil {
		return Result{Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+s.Token(r.As))
	if r.Body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.Method != http.MethodGet {
		if r.Key == "" {
			r.Key = uuid.NewString()
		}
		req.Header.Set("Idempotency-Key", r.Key)
	}
	if r.IfMatch != "" {
		req.Header.Set("If-Match", r.IfMatch)
	}
	start := time.Now()
	resp, err := s.Client.Do(req)
	if err != nil {
		return Result{Took: time.Since(start), Err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	res := Result{Status: resp.StatusCode, Body: body, ETag: resp.Header.Get("ETag"), Took: time.Since(start), Err: err}
	if err == nil && len(body) > 0 && !json.Valid(body) {
		res.Err = fmt.Errorf("%s %s: %d with a body that is not JSON (%d bytes)", r.Method, r.Path, resp.StatusCode, len(body))
	}
	return res
}

// Must performs a request that has to succeed.
func (s *Stack) Must(ctx context.Context, r Req) Result {
	s.T.Helper()
	res := s.Call(ctx, r)
	if !res.OK() {
		s.T.Fatalf("%s %s: %d %v %s", r.Method, r.Path, res.Status, res.Err, res.Body)
	}
	return res
}

// Entry is a journal entry of lines on company 2000: debit the expense
// account and credit the bank, 1.00 EUR a line, dated in period 6, with the
// key as its reference.
func (s *Stack) Entry(lines int, key string) string {
	parts := make([]string, 0, lines)
	for i := 0; i < lines; i++ {
		account, amount := s.Expense, "1.00"
		if i%2 == 1 {
			account, amount = s.Bank, "-1.00"
		}
		parts = append(parts, fmt.Sprintf(`{"account_id": %q, "amount": {"amount": %q, "currency": "EUR"}}`, account, amount))
	}
	return fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-06-15", "posting_date": "2026-06-15", "currency": "EUR", "reference": %q, "lines": [%s]}`,
		s.Env.DE, key, strings.Join(parts, ", "))
}

// Posted is every journal entry the users posted so far.
func (s *Stack) Posted() []Posted {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Posted(nil), s.posted...)
}

func (s *Stack) record(p Posted) {
	s.mu.Lock()
	s.posted = append(s.posted, p)
	s.mu.Unlock()
}

// Count runs a count query as superuser. A test that kills the database's
// connections kills this one too; it connects again.
func (s *Stack) Count(sql string, args ...any) int {
	s.T.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := context.Background()
	var n int
	err := s.admin.QueryRow(ctx, sql, args...).Scan(&n)
	if err != nil && s.admin.IsClosed() {
		if s.admin, err = pgx.Connect(ctx, pgtest.URL(s.T)); err == nil {
			err = s.admin.QueryRow(ctx, sql, args...).Scan(&n)
		}
	}
	if err != nil {
		s.T.Fatalf("%v\n%s", err, sql)
	}
	return n
}

// Backlogs are the entries the projector has not reached and the events the
// relay has not published, for the stack's tenant.
func (s *Stack) Backlogs() (projection, outbox int) {
	projection = s.Count(`SELECT count(*) FROM ledger.balance_projection WHERE tenant_id = $1 AND projected_at IS NULL`, s.Env.Tenant.ID)
	outbox = s.Count(`SELECT count(*) FROM ledger.outbox WHERE tenant_id = $1 AND published_at IS NULL`, s.Env.Tenant.ID) +
		s.Count(`SELECT count(*) FROM workflow.outbox WHERE tenant_id = $1 AND published_at IS NULL`, s.Env.Tenant.ID) +
		s.Count(`SELECT count(*) FROM procurement.outbox WHERE tenant_id = $1 AND published_at IS NULL`, s.Env.Tenant.ID)
	return projection, outbox
}

// Drain waits until the workers have caught up: the projection backlog is
// empty, and the outbox backlog too when the relay runs.
func (s *Stack) Drain(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		p, o := s.Backlogs()
		if (p == 0 || s.Projector == nil) && (o == 0 || s.Relay == nil) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("backlogs after %v: %d entries unprojected, %d events unpublished", timeout, p, o)
		}
		if p > 0 && s.Projector != nil {
			// An entry that committed after the projector's last round (a request
			// in flight when the load stopped) is projected here rather than
			// counted against the run.
			if _, err := s.Projector.RunOnce(context.Background()); err != nil {
				return fmt.Errorf("projecting the backlog: %w", err)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Consistent checks the ledger after a run: every reference posted exactly
// once, every entry the users were told about present, the projected
// balances equal to the journal, and the audit chain intact.
func (s *Stack) Consistent(ctx context.Context) error {
	var errs []string
	ten := s.Env.Tenant.ID
	if n := s.Count(`SELECT count(*) FROM (SELECT reference FROM ledger.journal_entry WHERE tenant_id = $1 AND reference IS NOT NULL
		GROUP BY reference HAVING count(*) > 1) d`, ten); n > 0 {
		errs = append(errs, fmt.Sprintf("%d references posted more than once", n))
	}
	var created []uuid.UUID
	for _, p := range s.Posted() {
		if p.Status != http.StatusCreated {
			continue
		}
		id, err := uuid.Parse(p.ID)
		if err != nil { // a 201 whose body was lost with the connection: counted, not fatal to the check
			errs = append(errs, fmt.Sprintf("an entry answered 201 without a readable id (%q)", p.ID))
			continue
		}
		created = append(created, id)
	}
	if missing := s.Count(`SELECT count(*) FROM unnest($2::uuid[]) AS c(id) WHERE NOT EXISTS (SELECT 1 FROM ledger.journal_entry e WHERE e.tenant_id = $1 AND e.id = c.id)`,
		ten, created); missing > 0 {
		errs = append(errs, fmt.Sprintf("%d entries answered 201 are not in the journal", missing))
	}
	if s.Projector != nil {
		if n := s.Count(`SELECT count(*) FROM (
			SELECT legal_entity_id, account_id, fiscal_period, sum(greatest(amount_lc, 0)) AS debit, sum(greatest(-amount_lc, 0)) AS credit
			FROM ledger.journal_line WHERE tenant_id = $1 GROUP BY 1, 2, 3
			EXCEPT SELECT legal_entity_id, account_id, fiscal_period, sum(debit_lc), sum(credit_lc) FROM ledger.period_balance WHERE tenant_id = $1 GROUP BY 1, 2, 3) d`, ten); n > 0 {
			errs = append(errs, fmt.Sprintf("%d account periods whose balance differs from the journal", n))
		}
	}
	res := s.Call(ctx, Req{Method: "POST", Path: "/v1/audit-events:verify", Body: `{}`, As: s.Env.Tenant.Human})
	if !res.OK() {
		errs = append(errs, fmt.Sprintf("audit verification: %d %v %s", res.Status, res.Err, res.Body))
	} else if r, _ := res.JSON()["result"].(map[string]any); r["verified"] != true {
		errs = append(errs, fmt.Sprintf("audit chain broken: %v", r))
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// User is a virtual user with its own principal.
type User struct {
	s     *Stack
	Actor pgtest.Actor
	n     int
	mu    sync.Mutex
	ids   []string // journal entries this user posted
}

// Operations of the mix, with the p95 and p99 budgets of 01-requirements
// §1.3. Setup requests (a requisition created and submitted so that it can
// be approved) are measured as "setup" and have no budget.
var Budgets = map[string][2]time.Duration{
	"read":    {50 * time.Millisecond, 150 * time.Millisecond},
	"list":    {150 * time.Millisecond, 400 * time.Millisecond},
	"post":    {200 * time.Millisecond, 500 * time.Millisecond},
	"approve": {150 * time.Millisecond, 400 * time.Millisecond},
	"balance": {time.Second, 3 * time.Second},
}

// Operations in the order reports list them.
var Operations = []string{"read", "list", "post", "approve", "balance", "setup"}

// Mix is the share of each operation per hundred steps: reads lead, as in
// an ERP where agents read heavily (05-scalability §5.1).
var Mix = []struct {
	Op     string
	Weight int
}{{"read", 40}, {"list", 20}, {"post", 25}, {"approve", 10}, {"balance", 5}}

// pick is the operation of step i.
func pick(i int) string {
	n := i % 100
	for _, m := range Mix {
		if n < m.Weight {
			return m.Op
		}
		n -= m.Weight
	}
	return Mix[0].Op
}

// Step does one operation of the mix and records it.
func (u *User) Step(ctx context.Context, st *Stats, i int) {
	s := u.s
	switch op := pick(i); op {
	case "read":
		u.mu.Lock()
		n := len(u.ids)
		var id string
		if n > 0 {
			id = u.ids[i%n]
		}
		u.mu.Unlock()
		if id == "" {
			u.post(ctx, st)
			return
		}
		st.Add("read", s.Call(ctx, Req{Method: "GET", Path: "/v1/journal-entries/" + id, As: u.Actor}))
	case "list":
		st.Add("list", s.Call(ctx, Req{Method: "GET", Path: "/v1/journal-entries?legal_entity_id=" + s.Env.DE.String() + "&fiscal_year=2026&page_size=50", As: u.Actor}))
	case "post":
		u.post(ctx, st)
	case "balance":
		st.Add("balance", s.Call(ctx, Req{Method: "GET", Path: "/v1/balances?legal_entity_id=" + s.Env.DE.String() + "&fiscal_year=2026&fiscal_period=6&page_size=500", As: u.Actor}))
	case "approve":
		u.approve(ctx, st)
	}
}

func (u *User) post(ctx context.Context, st *Stats) {
	key := uuid.NewString()
	res := u.s.Call(ctx, Req{Method: "POST", Path: "/v1/journal-entries", Body: u.s.Entry(20, key), As: u.Actor, Key: key})
	st.Add("post", res)
	p := Posted{Key: key, Status: res.Status, Err: res.Err}
	if res.OK() {
		p.ID, _ = res.JSON()["id"].(string)
		u.mu.Lock()
		u.ids = append(u.ids, p.ID)
		u.mu.Unlock()
	}
	u.s.record(p)
}

// approve creates and submits a requisition (setup) and has an approver
// find and approve it: a list and the measured decision.
func (u *User) approve(ctx context.Context, st *Stats) {
	s := u.s
	body := fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "items": [{"description": "Paper", "quantity": {"value": "10", "unit": "EA"},
		"unit_price": {"amount": "50.00", "currency": "EUR"}, "account_id": %q}]}`, s.Env.DE, s.Office)
	created := s.Call(ctx, Req{Method: "POST", Path: "/v1/requisitions", Body: body, As: u.Actor})
	st.Add("setup", created)
	if !created.OK() {
		return
	}
	id, _ := created.JSON()["id"].(string)
	submitted := s.Call(ctx, Req{Method: "POST", Path: "/v1/requisitions/" + id + ":submit", As: u.Actor, IfMatch: created.ETag})
	st.Add("setup", submitted)
	if !submitted.OK() {
		return
	}
	approver := s.Env.Approver
	if u.n%2 == 1 {
		approver = s.Env.Approver2
	}
	pending := s.Call(ctx, Req{Method: "GET", Path: "/v1/approval-requests?status=pending&object_id=" + id, As: approver})
	st.Add("list", pending)
	items, _ := pending.JSON()["items"].([]any)
	if !pending.OK() || len(items) != 1 {
		return
	}
	req := items[0].(map[string]any)
	version, _ := json.Marshal(req["version"])
	st.Add("approve", s.Call(ctx, Req{Method: "POST", Path: "/v1/approval-requests/" + req["id"].(string) + ":approve", Body: `{"comment": "ok"}`,
		As: approver, IfMatch: `"` + string(version) + `"`}))
}

// Run has every user step through the mix until the duration ends; rps
// above zero paces the users together at that many requests per second.
func (s *Stack) Run(ctx context.Context, d time.Duration, rps int, st *Stats) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	var pace <-chan time.Time
	if rps > 0 {
		t := time.NewTicker(time.Second / time.Duration(rps))
		defer t.Stop()
		pace = t.C
	}
	var wg sync.WaitGroup
	for _, u := range s.Users {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := u.n * 7; ctx.Err() == nil; i++ {
				if pace != nil {
					select {
					case <-pace:
					case <-ctx.Done():
						return
					}
				}
				u.Step(ctx, st, i)
			}
		}()
	}
	wg.Wait()
}

// Stats collects the samples of a run.
type Stats struct {
	mu      sync.Mutex
	took    map[string][]time.Duration
	codes   map[string]map[int]int
	errors  []string
	started time.Time
	failed  time.Time // the last failure
}

// NewStats starts collecting.
func NewStats() *Stats {
	return &Stats{took: map[string][]time.Duration{}, codes: map[string]map[int]int{}, started: time.Now()}
}

// Add records a result. Requests cut short by the run's end are not samples.
func (st *Stats) Add(op string, r Result) {
	if r.Err != nil && (strings.Contains(r.Err.Error(), "context deadline exceeded") || strings.Contains(r.Err.Error(), "context canceled")) {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.codes[op] == nil {
		st.codes[op] = map[int]int{}
	}
	if r.Err != nil {
		st.codes[op][0]++
		st.failed = time.Now()
		if len(st.errors) < 5 {
			st.errors = append(st.errors, op+": "+r.Err.Error())
		}
		return
	}
	st.codes[op][r.Status]++
	if r.Status < 400 {
		st.took[op] = append(st.took[op], r.Took)
		return
	}
	st.failed = time.Now()
	if len(st.errors) < 5 {
		st.errors = append(st.errors, fmt.Sprintf("%s: %d %s", op, r.Status, r.Body))
	}
}

// LastFailure is when the last request failed; zero when none did.
func (st *Stats) LastFailure() time.Time {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.failed
}

// Percentile is the pct-th percentile of the operation's successful
// requests, or 0 without samples.
func (st *Stats) Percentile(op string, pct float64) time.Duration {
	st.mu.Lock()
	defer st.mu.Unlock()
	ts := append([]time.Duration(nil), st.took[op]...)
	if len(ts) == 0 {
		return 0
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i] < ts[j] })
	i := int(float64(len(ts)-1) * pct / 100)
	return ts[i]
}

// Count is the number of successful samples of an operation.
func (st *Stats) Count(op string) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.took[op])
}

// Total is the number of requests, successful or not.
func (st *Stats) Total() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	n := 0
	for _, c := range st.codes {
		for _, k := range c {
			n += k
		}
	}
	return n
}

// Failures counts requests that failed or were refused, by operation and
// status (0: transport error). Throttled requests (429) are listed too.
func (st *Stats) Failures() map[string]int {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := map[string]int{}
	for op, c := range st.codes {
		for code, n := range c {
			if code >= 400 || code == 0 {
				out[fmt.Sprintf("%s %d", op, code)] += n
			}
		}
	}
	return out
}

// Errors are the first failures seen.
func (st *Stats) Errors() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]string(nil), st.errors...)
}

// Report logs a table of the run.
func (st *Stats) Report(t testing.TB, title string) {
	t.Helper()
	elapsed := time.Since(st.started)
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d requests in %v (%.0f/s)\n", title, st.Total(), elapsed.Round(time.Millisecond), float64(st.Total())/elapsed.Seconds())
	fmt.Fprintf(&b, "  %-8s %7s %10s %10s %10s %10s\n", "op", "n", "p50", "p95", "p99", "budget p95")
	for _, op := range Operations {
		if st.Count(op) == 0 {
			continue
		}
		budget := "-"
		if bd, ok := Budgets[op]; ok {
			budget = Budget(bd[0]).String() // as checked, scaled by ERP_BUDGET_SCALE
		}
		fmt.Fprintf(&b, "  %-8s %7d %10v %10v %10v %10s\n", op, st.Count(op), st.Percentile(op, 50).Round(time.Millisecond),
			st.Percentile(op, 95).Round(time.Millisecond), st.Percentile(op, 99).Round(time.Millisecond), budget)
	}
	if f := st.Failures(); len(f) > 0 {
		fmt.Fprintf(&b, "  failures: %v\n", f)
		for _, e := range st.Errors() {
			fmt.Fprintf(&b, "    %s\n", e)
		}
	}
	t.Log(b.String())
}

// CheckBudgets fails the test for every operation over its p95 or p99
// budget, and for any failed request.
func (st *Stats) CheckBudgets(t testing.TB) {
	t.Helper()
	for op, b := range Budgets {
		if st.Count(op) == 0 {
			t.Errorf("%s: no samples", op)
			continue
		}
		if p95, p99 := st.Percentile(op, 95), st.Percentile(op, 99); p95 > Budget(b[0]) || p99 > Budget(b[1]) {
			t.Errorf("%s: p95 %v (budget %v), p99 %v (budget %v)", op, p95.Round(time.Millisecond), Budget(b[0]), p99.Round(time.Millisecond), Budget(b[1]))
		}
	}
	if f := st.Failures(); len(f) > 0 {
		t.Errorf("failed requests: %v\n%s", f, strings.Join(st.Errors(), "\n"))
	}
}

// Setting reads a duration or number from the environment, for the knobs
// of the suites (ERP_LOAD_USERS, ERP_LOAD_DURATION, ...).
func Setting(name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		panic(fmt.Sprintf("%s=%q: want a duration such as 30s or a number", name, v))
	}
	return time.Duration(n)
}

// BudgetScale is the factor ERP_BUDGET_SCALE applies to every latency
// budget (default 1). The budgets of 01-requirements §1.3 are a cell's; a
// shared CI runner with the database in a sidecar container is several
// times slower, so its runs scale them to catch regressions rather than
// fail on the machine. A scale below 1 tightens them.
func BudgetScale() float64 {
	v := os.Getenv("ERP_BUDGET_SCALE")
	if v == "" {
		return 1
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		panic(fmt.Sprintf("ERP_BUDGET_SCALE=%q: want a positive number such as 4", v))
	}
	return f
}

// Budget scales a latency budget by BudgetScale.
func Budget(d time.Duration) time.Duration {
	return time.Duration(float64(d) * BudgetScale()).Round(time.Millisecond)
}

// contains reports whether s has sub.
func contains(s, sub string) bool { return strings.Contains(s, sub) }

// natstestServer starts an embedded JetStream server for a test.
func natstestServer(t testing.TB) string { return natstest.Server(t) }
