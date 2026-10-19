package payments_test

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
	"github.com/mustie7iy/agentic-erp/internal/platform/sftpclient/sftptest"
)

// sftpSettings are bank_sftp settings for a test server's root directory.
func sftpSettings(s *sftptest.Server) string {
	host, port, _ := net.SplitHostPort(s.Addr) // the listener's own address
	return fmt.Sprintf(`{"host": %q, "port": %s, "user": "erp", "host_key": %q, "directory": "/"}`, host, port, strings.TrimSpace(s.HostKey()))
}

// channelDoc is the configuration with company 2000's main house bank on
// the named connector.
func channelDoc(t *testing.T, connector string) *config.Config {
	t.Helper()
	doc := fixture.Document(t)
	doc.Companies[1].HouseBank.Connector = connector
	return doc
}

// TestHouseBankChannel: a house bank names the connector its files go
// through (ADR-0038, T44b); the file is uploaded there, the event names the
// connector, and a channel that cannot be used is refused with nothing sent.
func TestHouseBankChannel(t *testing.T) {
	key, client := sftptest.PrivateKey(t)
	vars := map[string]string{"BANK_KEY": string(key)}
	e := setupWith(t, nil, func(cfg *app.Config) { cfg.Secrets = secrets.Local{Getenv: func(n string) string { return vars[n] }} })
	server := sftptest.NewServer(t, client.PublicKey(), func(*sftptest.Server) {})
	conn := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO integration.connector (tenant_id, id, kind, name, settings, secret_ref, status, created_by, updated_by)
		VALUES ($1, $2, 'bank_sftp', 'Bank SFTP', $3, 'env:BANK_KEY', 'active', $4, $4),
			($1, gen_random_uuid(), 'sap_file', 'SAP', '{"path": "x.json"}', NULL, 'active', $4, $4)`, e.Tenant.ID, conn, sftpSettings(server), e.Tenant.Human.ID)
	h := houseBanks()
	t.Run("configuration", func(t *testing.T) { checkChannelConfig(t, e, h, conn) })

	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
	run := e.run(t, "2026-04-15", "2026-04-15")
	e.release(t, run, "")
	files, _ := e.Must(200, "GET", "/v1/bank-files?payment_run_id="+id(run), "", "")
	file := files["items"].([]any)[0].(map[string]any)
	path := "/v1/bank-files/" + id(file) + ":transmit"

	// Without a secret for its key the channel is refused; so is a server
	// that does not take the file. Nothing is transmitted either way.
	pgtest.Exec(t, `UPDATE integration.connector SET secret_ref = NULL, version = version + 1 WHERE id = $1`, conn)
	e.Problem(422, "integration.secret_required", "POST", path, "", `"1"`)
	pgtest.Exec(t, `UPDATE integration.connector SET secret_ref = 'env:BANK_KEY', settings = $2, version = version + 1 WHERE id = $1`, conn,
		sftpSettings(sftptest.NewServer(t, client.PublicKey(), func(s *sftptest.Server) { s.FailRename = true })))
	e.Problem(503, "platform.unavailable", "POST", path, "", `"1"`)
	if got, _ := e.Must(200, "GET", "/v1/bank-files/"+id(file), "", ""); got["status"] != "created" {
		t.Fatalf("file after refusals, status: got %v, want created", got["status"])
	}

	pgtest.Exec(t, `UPDATE integration.connector SET settings = $2, version = version + 1 WHERE id = $1`, conn, sftpSettings(server))
	e.Must(202, "POST", path, "", `"1"`)
	name := "/2000-" + run["document_number"].(string) + "-" + id(file) + ".xml"
	if content, ok := server.Read(t, name); !ok || !strings.Contains(content, "<IBAN>DE89370400440532013000</IBAN>") {
		t.Errorf("uploaded file %s: got %q (found %v), want the pain.001", name, content, ok)
	}
	if len(e.connector.files) != 0 {
		t.Errorf("files in the server's directory: got %d, want none", len(e.connector.files))
	}
	if ev := events(t, e.Tenant, "bank_file_transmitted"); len(ev) != 1 || ev[0]["connector_id"] != conn.String() {
		t.Errorf("transmitted events: got %v, want one naming connector %s", ev, conn)
	}
}

// checkChannelConfig: the configuration names a bank channel by its name,
// and applying it records the connector on the house bank.
func checkChannelConfig(t *testing.T, e env, h payments.HouseBanks, conn uuid.UUID) {
	t.Helper()
	for name, c := range map[string]struct{ connector, want string }{
		"a bank channel":     {"Bank SFTP", ""},
		"no such connector":  {"Nobody", "companies[2000].house_bank.connector"},
		"not a bank channel": {"SAP", "companies[2000].house_bank.connector"},
	} {
		t.Run(name, func(t *testing.T) {
			var findings []config.Finding
			if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
				findings, err = h.Check(ctx, nil, channelDoc(t, c.connector))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var paths []string
			for _, f := range findings {
				paths = append(paths, f.Path)
			}
			if got := strings.Join(paths, ","); got != c.want {
				t.Errorf("findings: got %q, want %q", got, c.want)
			}
		})
	}
	apply := func(doc *config.Config) error {
		return apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return h.Apply(ctx, doc) })
	}
	if err := apply(channelDoc(t, "Nobody")); err == nil || !strings.Contains(err.Error(), "no bank channel connector is named Nobody") {
		t.Errorf("applied a connector renamed since the check: got %v, want an error", err)
	}
	if err := apply(channelDoc(t, "Bank SFTP")); err != nil {
		t.Fatal(err)
	}
	var got *uuid.UUID
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT connector_id FROM payments.house_bank WHERE id = $1`, e.bank).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != conn {
		t.Errorf("house bank connector: got %v, want %s", got, conn)
	}
	t.Run("every statement failing", func(t *testing.T) {
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "check", func(ctx context.Context) error {
			_, err := h.Check(ctx, nil, channelDoc(t, "Bank SFTP"))
			return err
		})
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "apply", func(ctx context.Context) error { return h.Apply(ctx, channelDoc(t, "Bank SFTP")) })
	})
}
