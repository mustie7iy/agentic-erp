package workflows_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore/blobstoretest"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
	"github.com/mustie7iy/agentic-erp/internal/platform/sftpclient/sftptest"
	"github.com/mustie7iy/agentic-erp/internal/workflows"
)

// statement is an MT940 statement of house bank MAIN of company 2000
// (DE89370400440532013000, EUR) with no lines.
func statement(ref, seq, balance, date string) string {
	return ":20:" + ref + "\n:25:DE89370400440532013000\n:28C:" + seq + "\n:60F:C" + date + "EUR" + balance + "\n:62F:C" + date + "EUR" + balance + "\n"
}

// fetchScenario is a tenant whose house bank MAIN fetches statements from
// an in-process SFTP server's /in, archived to /archive.
type fetchScenario struct {
	e      env
	a      *workflows.Statements
	server *sftptest.Server
	in     workflows.FetchInput
	conn   uuid.UUID
}

func fetchSetup(t *testing.T) *fetchScenario {
	t.Helper()
	e := setup(t)
	key, client := sftptest.PrivateKey(t)
	server := sftptest.NewServer(t, client.PublicKey(), func(*sftptest.Server) {})
	server.Mkdir(t, "/in")
	server.Mkdir(t, "/archive")
	host, port, _ := net.SplitHostPort(server.Addr) // the listener's own address
	conn := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO integration.connector (tenant_id, id, kind, name, settings, secret_ref, status, created_by, updated_by)
		VALUES ($1, $2, 'bank_sftp', 'Bank SFTP', $3, 'env:BANK_KEY', 'active', $4, $4)`, e.Tenant.ID, conn,
		fmt.Sprintf(`{"host": %q, "port": %s, "user": "erp", "host_key": %q, "directory": "/", "statements_directory": "/in", "statements_archive": "/archive"}`,
			host, port, strings.TrimSpace(server.HostKey())), e.Tenant.Human.ID)
	pgtest.Exec(t, `UPDATE payments.house_bank SET connector_id = $2, version = version + 1 WHERE id = $1`, e.bank, conn)
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	h := app.Payments(app.Config{Pages: pages, FieldKeys: apitest.FieldKeys(), Objects: blobstoretest.New(t).Store(),
		Secrets: secrets.Local{Getenv: func(n string) string { return map[string]string{"BANK_KEY": string(key)}[n] }}})
	return &fetchScenario{e: e, server: server, conn: conn, in: workflows.FetchInput{TenantID: e.Tenant.ID, HouseBankID: e.bank},
		a: &workflows.Statements{Pool: pgtest.Pool(t), AppRole: "erp_app", Endpoints: h}}
}

// run runs the fetch workflow with the real activities; replace, when
// given, stands in for the import of the files it names.
func (s *fetchScenario) run(t *testing.T, replace map[string]error) error {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := workflowEnv(&suite)
	env.RegisterWorkflow(workflows.FetchBankStatements)
	env.RegisterActivity(s.a)
	if replace != nil {
		env.OnActivity(s.a.ImportStatementFile, mock.Anything, mock.Anything).Return(func(ctx context.Context, in workflows.FileInput) (payments.Fetched, error) {
			if err, ok := replace[in.Name]; ok {
				return payments.Fetched{}, err
			}
			return s.a.ImportStatementFile(ctx, in)
		})
	}
	env.ExecuteWorkflow(workflows.FetchBankStatements, s.in)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	return env.GetWorkflowError()
}

// fetchState is the recorded status of the bank's file with this content.
func (s *fetchScenario) fetchState(t *testing.T, content string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	var status string
	err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT status FROM payments.statement_fetch WHERE house_bank_id = $1 AND file_sha256 = $2`,
		s.e.bank, sum[:]).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return status
}

// archived is the archive path of a file of the bank.
func archived(name, content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("/archive/%x-%s", sum[:8], name)
}

// otherBank adds an active house bank of company 2000 with the account,
// in EUR, on the scenario's channel; sealed, when given, stands for its
// sealed IBAN.
func (s *fetchScenario) otherBank(t *testing.T, iban string, sealed ...[]byte) uuid.UUID {
	t.Helper()
	cipher := apitest.Seal(t, s.e.Tenant.ID, iban)
	if len(sealed) > 0 {
		cipher = sealed[0]
	}
	bank := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO payments.house_bank (tenant_id, id, legal_entity_id, code, name, iban_cipher, iban_last4, bic, currency, gl_account_id,
		active_gl_account_id, status, connector_id, created_by, updated_by) VALUES ($1, $2, $3, 'DB', 'DB', $4, $5, 'DEUTDEFFXXX', 'EUR', $6, $6, 'active', $7, $8, $8)`,
		s.e.Tenant.ID, bank, s.e.DE, cipher, iban[len(iban)-4:], s.e.Account(t, "2000", "101000"), s.conn, s.e.Tenant.Human.ID)
	return bank
}

func (s *fetchScenario) count(t *testing.T, name string) int {
	t.Helper()
	return len(apitest.CompanyNamed(t, apitest.Published(t, s.e.Tenant, outbox.Event{Context: "payments", Name: name})))
}

// TestFetchStatements: the house bank's statements are fetched from its
// channel, imported through the upload's path and archived; a file refused,
// or of no house bank on the channel, is recorded once and left, a file of
// another bank on the channel is left for it, and a second run imports and
// publishes nothing again (ADR-0047, T49d).
func TestFetchStatements(t *testing.T) {
	s := fetchSetup(t)
	files := map[string]string{
		"a.sta":      statement("A1", "1", "100,00", "261001"),
		"b.sta":      statement("A2", "2", "100,00", "261002"),
		"bad.sta":    strings.Replace(statement("A3", "3", "100,00", "261003"), ":62F:C261003EUR100,00", ":62F:C261003EUR1,00", 1),
		"other.sta":  strings.Replace(statement("A4", "4", "100,00", "261004"), "DE89370400440532013000", "GB33BUKB20201555555555", 1),
		"shared.sta": strings.Replace(statement("A7", "7", "100,00", "261004"), "DE89370400440532013000", "DE72500700100175526300", 1),
	}
	s.otherBank(t, "DE72500700100175526300")
	for name, content := range files {
		s.server.Put(t, "/in/"+name, content)
	}
	if err := s.run(t, nil); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	check := func(t *testing.T) {
		t.Helper()
		for name, want := range map[string]struct {
			status        string
			left, archive bool
		}{"a.sta": {"archived", false, true}, "b.sta": {"archived", false, true}, "bad.sta": {"refused", true, false}, "other.sta": {"refused", true, false},
			"shared.sta": {"", true, false}} {
			if got := s.fetchState(t, files[name]); got != want.status {
				t.Errorf("%s: got status %q, want %q", name, got, want.status)
			}
			if _, left := s.server.Read(t, "/in/"+name); left != want.left {
				t.Errorf("%s: got left in /in %v, want %v", name, left, want.left)
			}
			if got, ok := s.server.Read(t, archived(name, files[name])); ok != want.archive || (ok && got != files[name]) {
				t.Errorf("%s: got archived %v %q, want %v", name, ok, got, want.archive)
			}
		}
		for event, want := range map[string]int{"bank_statement_imported": 2, "bank_statement_file_refused": 2} {
			if got := s.count(t, event); got != want {
				t.Errorf("%s events: got %d, want %d", event, got, want)
			}
		}
	}
	check(t)
	t.Run("again", func(t *testing.T) {
		if err := s.run(t, nil); err != nil {
			t.Fatalf("fetch again: %v", err)
		}
		check(t)
	})
	t.Run("the same file delivered again", func(t *testing.T) {
		s.server.Put(t, "/in/a.sta", files["a.sta"])
		if err := s.run(t, nil); err != nil {
			t.Fatalf("fetch: %v", err)
		}
		check(t)
	})
	t.Run("one file's failure leaves the others", func(t *testing.T) {
		c, d := statement("A5", "5", "100,00", "261005"), statement("A6", "6", "100,00", "261006")
		s.server.Put(t, "/in/c.sta", c)
		s.server.Put(t, "/in/d.sta", d)
		err := s.run(t, map[string]error{"c.sta": temporal.NewNonRetryableApplicationError("bank unreachable", "test", nil)})
		if err == nil || !strings.Contains(err.Error(), "files not fetched: c.sta") {
			t.Errorf("got %v, want c.sta named as not fetched", err)
		}
		if got := s.fetchState(t, d); got != "archived" {
			t.Errorf("d.sta: got %q, want archived", got)
		}
		if got := s.fetchState(t, c); got != "" {
			t.Errorf("c.sta: got %q, want it left for the next fetch", got)
		}
	})
}

// TestFetchArchiveFails: a file whose move the server refuses stays
// imported, its workflow names it, and the next fetch archives it without
// importing it again.
func TestFetchArchiveFails(t *testing.T) {
	s := fetchSetup(t)
	content := statement("E1", "1", "7,00", "261001")
	s.server.Put(t, "/in/e.sta", content)
	s.server.FailRename = true
	if err := s.run(t, nil); err == nil || !strings.Contains(err.Error(), "files not fetched: e.sta") {
		t.Errorf("got %v, want e.sta named as not fetched", err)
	}
	if got := s.fetchState(t, content); got != "imported" {
		t.Errorf("after the refused move: got %q, want imported", got)
	}
	s.server.FailRename = false
	if err := s.run(t, nil); err != nil {
		t.Fatalf("next fetch: %v", err)
	}
	if got := s.fetchState(t, content); got != "archived" {
		t.Errorf("after the next fetch: got %q, want archived", got)
	}
	if got := s.count(t, "bank_statement_imported"); got != 1 {
		t.Errorf("imported events: got %d, want 1", got)
	}
}

// TestFetchChangedAfterImport: a file whose content changed on the server
// after its import is not archived; the change is recorded once with
// BankStatementFileChanged, and the next fetch imports the new content as a
// new file and archives it (coordinator 2026-10-20).
func TestFetchChangedAfterImport(t *testing.T) {
	s := fetchSetup(t)
	before, after := statement("C1", "1", "3,00", "261001"), statement("C2", "2", "3,00", "261002")
	s.server.Put(t, "/in/c.sta", before)
	in, ctx := workflows.FileInput{FetchInput: s.in, Name: "c.sta"}, context.Background()
	f, err := s.a.ImportStatementFile(ctx, in)
	if err != nil || f.Status != payments.FetchImported {
		t.Fatalf("import: got %v %v, want imported", f, err)
	}
	s.server.Put(t, "/in/c.sta", after)
	in.SHA256 = f.SHA256
	for i := range 2 {
		if err := s.a.ArchiveStatementFile(ctx, in); err != nil {
			t.Fatalf("archive %d: %v", i+1, err)
		}
	}
	if got := s.fetchState(t, before); got != payments.FetchChanged {
		t.Errorf("the content imported: got %q, want %s", got, payments.FetchChanged)
	}
	if got, ok := s.server.Read(t, "/in/c.sta"); !ok || got != after {
		t.Errorf("the file: got %v %q, want the new content left", ok, got)
	}
	if _, ok := s.server.Read(t, archived("c.sta", before)); ok {
		t.Error("the file was archived under the content imported")
	}
	if got := s.count(t, "bank_statement_file_changed"); got != 1 {
		t.Errorf("changed events: got %d, want 1", got)
	}
	if err := s.run(t, nil); err != nil {
		t.Fatalf("next fetch: %v", err)
	}
	if got := s.fetchState(t, after); got != payments.FetchArchived {
		t.Errorf("the new content: got %q, want archived", got)
	}
	if got := s.count(t, "bank_statement_imported"); got != 2 {
		t.Errorf("imported events: got %d, want 2", got)
	}
	t.Run("the content imported delivered again", func(t *testing.T) {
		s.server.Put(t, "/in/c.sta", before)
		if err := s.run(t, nil); err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if got, ok := s.server.Read(t, archived("c.sta", before)); !ok || got != before {
			t.Errorf("archived: got %v %q, want the content imported", ok, got)
		}
		if got := s.count(t, "bank_statement_imported"); got != 2 {
			t.Errorf("imported events: got %d, want 2", got)
		}
	})
}

// TestFetchActivitiesRunTwice: each activity run again does nothing twice:
// a file imported but not archived when a fetch was cut short is not
// imported again, and an archiving whose record was lost after the move is
// finished by the retry.
func TestFetchActivitiesRunTwice(t *testing.T) {
	s := fetchSetup(t)
	content := statement("B1", "1", "5,00", "261001")
	s.server.Put(t, "/in/b.sta", content)
	in, ctx := workflows.FileInput{FetchInput: s.in, Name: "b.sta"}, context.Background()
	var f payments.Fetched
	for i := range 2 {
		var err error
		if f, err = s.a.ImportStatementFile(ctx, in); err != nil || f.Status != payments.FetchImported {
			t.Fatalf("import %d: got %v %v, want imported", i+1, f, err)
		}
	}
	if got := s.count(t, "bank_statement_imported"); got != 1 {
		t.Errorf("imported events: got %d, want 1", got)
	}
	in.SHA256 = f.SHA256
	calls := 0
	s.a.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
		if calls++; calls == 2 {
			return nil, errors.New("connection lost")
		}
		return s.a.Pool.Begin(ctx, scope)
	}
	if err := s.a.ArchiveStatementFile(ctx, in); err == nil {
		t.Fatal("archive with its record lost: got no error")
	}
	if got := s.fetchState(t, content); got != "imported" {
		t.Errorf("after the move: got %q, want still imported", got)
	}
	s.a.Begin = nil
	for i := range 2 {
		if err := s.a.ArchiveStatementFile(ctx, in); err != nil {
			t.Fatalf("archive %d: %v", i+1, err)
		}
	}
	if got, _ := s.server.Read(t, archived("b.sta", content)); got != content {
		t.Errorf("archived file: got %q, want %q", got, content)
	}
	if got := s.fetchState(t, content); got != "archived" {
		t.Errorf("status: got %q, want archived", got)
	}
	bad := "x"
	s.server.Put(t, "/in/x.sta", bad)
	for i := range 2 {
		if f, err := s.a.ImportStatementFile(ctx, workflows.FileInput{FetchInput: s.in, Name: "x.sta"}); err != nil || f.Status != payments.FetchRefused {
			t.Fatalf("refusal %d: got %v %v, want refused", i+1, f, err)
		}
	}
	if got := s.count(t, "bank_statement_file_refused"); got != 1 {
		t.Errorf("refused events: got %d, want 1", got)
	}
}

// TestFetchFailures: a bank that cannot fetch fails its workflow without
// retries; the server's and the database's failures are returned for the
// retry policy.
func TestFetchFailures(t *testing.T) {
	s := fetchSetup(t)
	ctx := context.Background()
	in := workflows.FileInput{FetchInput: s.in, Name: "f.sta"}
	t.Run("no statement directories", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE integration.connector SET settings = settings - 'statements_directory' - 'statements_archive', version = version + 1 WHERE id = $1`, s.conn)
		defer pgtest.Exec(t, `UPDATE integration.connector SET settings = settings || '{"statements_directory": "/in", "statements_archive": "/archive"}', version = version + 1 WHERE id = $1`, s.conn)
		err := s.run(t, nil)
		var refused *temporal.ApplicationError
		if !errors.As(err, &refused) || !refused.NonRetryable() || !strings.Contains(err.Error(), "statements_directory") {
			t.Errorf("workflow: got %v, want the bank refused without retries", err)
		}
		if _, err := s.a.ImportStatementFile(ctx, in); err == nil {
			t.Error("import: got no error")
		}
		if err := s.a.ArchiveStatementFile(ctx, in); err == nil {
			t.Error("archive: got no error")
		}
	})
	for name, c := range map[string]struct {
		state func()
		call  func() error
		want  string
	}{
		"no house bank channel": {func() {
			pgtest.Exec(t, `UPDATE payments.house_bank SET connector_id = NULL, version = version + 1 WHERE id = $1`, s.e.bank)
		},
			func() error { _, err := s.a.ListStatementFiles(ctx, s.in); return err }, "payments.no_statement_channel"},
		"a disabled channel": {func() {
			pgtest.Exec(t, `UPDATE integration.connector SET status = 'disabled', version = version + 1 WHERE id = $1`, s.conn)
		},
			func() error { _, err := s.a.ListStatementFiles(ctx, s.in); return err }, "disabled"},
		"listing refused": {func() { s.server.FailList = true }, func() error { _, err := s.a.ListStatementFiles(ctx, s.in); return err }, "list /in"},
		"no such file": {func() {}, func() error {
			_, err := s.a.ImportStatementFile(ctx, workflows.FileInput{FetchInput: s.in, Name: "none.sta"})
			return err
		}, "read statement file none.sta"},
		"rename refused": {func() { s.server.FailRename = true; s.server.Put(t, "/in/f.sta", "f") },
			func() error {
				sum := sha256.Sum256([]byte("f"))
				return s.a.ArchiveStatementFile(ctx, workflows.FileInput{FetchInput: s.in, Name: "f.sta", SHA256: sum[:]})
			}, "archive statement file f.sta"},
		"reading again refused": {func() { s.server.FailRead = true; s.server.Put(t, "/in/f.sta", "f") },
			func() error {
				return s.a.ArchiveStatementFile(ctx, workflows.FileInput{FetchInput: s.in, Name: "f.sta", SHA256: make([]byte, 32)})
			},
			"read statement file f.sta of house bank"},
		"the database": {func() {
			s.server.Put(t, "/in/f.sta", statement("F1", "1", "1,00", "261001"))
			s.a.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
		}, func() error { _, err := s.a.ImportStatementFile(ctx, in); return err }, "database down"},
	} {
		t.Run(name, func(t *testing.T) {
			c.state()
			defer func() {
				s.server.FailList, s.server.FailRename, s.server.FailRead, s.a.Begin = false, false, false, nil
				pgtest.Exec(t, `UPDATE payments.house_bank SET connector_id = $2, version = version + 1 WHERE id = $1`, s.e.bank, s.conn)
				pgtest.Exec(t, `UPDATE integration.connector SET status = 'active', version = version + 1 WHERE id = $1`, s.conn)
			}()
			if err := c.call(); err == nil || !strings.Contains(fmt.Sprint(err, " ", problem.From(err).Code), c.want) {
				t.Errorf("got %v, want an error naming %s", err, c.want)
			}
		})
	}
	t.Run("a refusal not recorded", func(t *testing.T) {
		s.server.Put(t, "/in/g.sta", "g")
		calls := 0
		s.a.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
			if calls++; calls == 3 { // the channel, the import, then the refusal
				return nil, errors.New("database down")
			}
			return s.a.Pool.Begin(ctx, scope)
		}
		defer func() { s.a.Begin = nil }()
		if _, err := s.a.ImportStatementFile(ctx, workflows.FileInput{FetchInput: s.in, Name: "g.sta"}); err == nil {
			t.Error("got no error")
		}
	})
}

// TestFetchDatabaseFailures: every statement of the fetch's records fails
// in turn, and none runs outside a unit of work.
func TestFetchDatabaseFailures(t *testing.T) {
	s := fetchSetup(t)
	h, bank, ctx := s.a.Endpoints, s.e.bank, context.Background()
	camt := `<?xml version="1.0"?><Document/>`
	file := []byte(statement("D1", "1", "1,00", "261001"))
	sum := sha256.Sum256(file)
	refusal := problem.Unprocessable("payments.statement_unreadable", "unreadable", problem.FieldError{Field: "file", Code: "unreadable", Message: "line 1"})
	for name, fn := range map[string]func(ctx context.Context) error{
		"channel": func(ctx context.Context) error { _, err := h.StatementChannel(ctx, bank); return err },
		"import":  func(ctx context.Context) error { _, err := h.ImportFetched(ctx, bank, "d.sta", file); return err },
		"camt": func(ctx context.Context) error {
			_, err := h.ImportFetched(ctx, bank, "d.xml", []byte(camt))
			return ignore(err, "payments.statement_unreadable")
		},
		"refuse": func(ctx context.Context) error { return h.RefuseFetched(ctx, bank, "d.sta", sum[:], refusal) },
		"no bank's": func(ctx context.Context) error {
			_, err := h.ImportFetched(ctx, bank, "o.sta", []byte(strings.Replace(string(file), "DE89370400440532013000", "GB33BUKB20201555555555", 1)))
			return ignore(err, "payments.statement_of_no_bank")
		},
		"changed": func(ctx context.Context) error {
			if _, err := h.ImportFetched(ctx, bank, "d.sta", file); err != nil {
				return err
			}
			return h.ChangedFetched(ctx, bank, sum[:], make([]byte, 32))
		},
		"archive": func(ctx context.Context) error {
			if _, err := h.ImportFetched(ctx, bank, "d.sta", file); err != nil {
				return err
			}
			return payments.ArchivedFetched(ctx, bank, sum[:])
		},
	} {
		apitest.EveryFailure(t, s.e.Tenant, s.e.Tenant.Human, name, fn)
	}
	s.server.Put(t, "/in/d.sta", string(file))
	if err := s.run(t, nil); err != nil {
		t.Fatal(err)
	}
	for name, fn := range map[string]func(ctx context.Context) error{
		"import again":           func(ctx context.Context) error { _, err := h.ImportFetched(ctx, bank, "d.sta", file); return err },
		"refuse again":           func(ctx context.Context) error { return h.RefuseFetched(ctx, bank, "d.sta", sum[:], refusal) },
		"archive again":          func(ctx context.Context) error { return payments.ArchivedFetched(ctx, bank, sum[:]) },
		"changed after archived": func(ctx context.Context) error { return h.ChangedFetched(ctx, bank, sum[:], make([]byte, 32)) },
	} {
		apitest.EveryFailure(t, s.e.Tenant, s.e.Tenant.Human, name, fn)
	}
	for name, err := range map[string]error{
		"channel": func() error { _, err := h.StatementChannel(ctx, bank); return err }(),
		"import":  func() error { _, err := h.ImportFetched(ctx, bank, "d.sta", file); return err }(),
		"refuse":  h.RefuseFetched(ctx, bank, "d.sta", sum[:], refusal),
		"archive": payments.ArchivedFetched(ctx, bank, sum[:]),
		"changed": h.ChangedFetched(ctx, bank, sum[:], make([]byte, 32)),
	} {
		t.Run(name, func(t *testing.T) { pgtest.WantErr(t, err, "") })
	}
	t.Run("a sharing bank's IBAN that does not open", func(t *testing.T) {
		s.otherBank(t, "DE72500700100175526300", []byte{0})
		err := apitest.InRequest(t, s.e.Tenant, s.e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.ImportFetched(ctx, bank, "o.sta", []byte(strings.Replace(string(file), "DE89370400440532013000", "GB33BUKB20201555555555", 1)))
			return err
		})
		pgtest.WantErr(t, err, "open the IBAN of a house bank sharing the channel")
	})
}

// ignore is err unless it is the problem with that code.
func ignore(err error, code string) error {
	if problem.From(err).Code == code {
		return nil
	}
	return err
}

// TestDispatchFetches: once per period the dispatcher starts a fetch for
// each active house bank whose channel names statement directories; a
// refused start is tried again in the same period.
func TestDispatchFetches(t *testing.T) {
	s := fetchSetup(t)
	engine := &fakeEngine{}
	now := time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)
	d := &workflows.Dispatcher{Pool: pgtest.Pool(t), WorkerRole: "erp_worker", Client: engine, Queue: "erp-test", Statements: time.Hour,
		Now: func() time.Time { return now }}
	ctx := context.Background()
	want := workflows.FetchWorkflowID(s.e.Tenant.ID, s.e.bank, time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC))
	ours := func() []string {
		var ids []string
		for _, st := range engine.started {
			if strings.HasPrefix(st.ID, s.e.Tenant.ID.String()+"-house_bank") {
				ids = append(ids, st.ID)
			}
		}
		return ids
	}
	engine.fail = want
	if _, err := d.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("refused start: got %v, want it reported", err)
	}
	engine.fail = ""
	if _, err := d.RunOnce(ctx); err != nil {
		t.Fatalf("round: %v", err)
	}
	if got := ours(); len(got) != 1 || got[0] != want {
		t.Fatalf("started: got %v, want %s", got, want)
	}
	if _, err := d.RunOnce(ctx); err != nil || len(ours()) != 1 {
		t.Errorf("same period: got %v %v, want nothing more", ours(), err)
	}
	now = now.Add(time.Hour)
	if _, err := d.RunOnce(ctx); err != nil || len(ours()) != 2 {
		t.Errorf("next period: got %v %v, want a second fetch", ours(), err)
	}
	t.Run("a channel without statement directories", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE integration.connector SET settings = settings - 'statements_directory' - 'statements_archive', version = version + 1 WHERE id = $1`, s.conn)
		pgtest.Exec(t, `INSERT INTO integration.connector (tenant_id, id, kind, name, settings, secret_ref, status, created_by, updated_by)
			SELECT tenant_id, gen_random_uuid(), kind, 'Statements only', settings || '{"statements_directory": "/in", "statements_archive": "/archive"}', secret_ref, status, created_by, updated_by
			FROM integration.connector WHERE id = $1`, s.conn)
		now = now.Add(time.Hour)
		if _, err := d.RunOnce(ctx); err != nil || len(ours()) != 2 {
			t.Errorf("got %v %v, want no fetch for a bank whose channel has no statements", ours(), err)
		}
	})
	t.Run("database failures", func(t *testing.T) {
		pool := pgtest.Pool(t)
		defer func() { d.Begin = nil }()
		for n := 1; n < 20; n++ {
			now = now.Add(time.Hour)
			count := 0
			d.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
				tx, err := pool.Begin(ctx, scope)
				if err != nil {
					return nil, err
				}
				return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, nil
			}
			_, err := d.RunOnce(ctx)
			if err == nil {
				break
			}
			if !errors.Is(err, pgtest.ErrInjected) {
				t.Fatalf("statement %d: %v", n, err)
			}
		}
	})
}
