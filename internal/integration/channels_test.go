package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
	"github.com/mustie7iy/agentic-erp/internal/platform/sftpclient"
	"github.com/mustie7iy/agentic-erp/internal/platform/sftpclient/sftptest"
)

// TestBankChannels: a house bank's connector resolves into the SFTP
// endpoint it names with the private key from its secret, and fails closed
// on anything else (ADR-0038, T44b).
func TestBankChannels(t *testing.T) {
	f := fixture.New(t)
	key, client := sftptest.PrivateKey(t)
	host := sftptest.Signer(t).PublicKey()
	vars := map[string]string{"BANK_KEY": string(key), "NOT_A_KEY": "password"}
	b := integration.BankChannels{Secrets: secrets.Local{Getenv: func(n string) string { return vars[n] }}}
	settings := `{"host": "sftp.bank.example", "port": 2222, "user": "erp", "host_key": "` + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(host))) + `", "directory": "/in"}`
	conn := func(kind, name, settings, ref, status string) uuid.UUID {
		id := uuid.Must(uuid.NewV7())
		var secret *string
		if ref != "" {
			secret = &ref
		}
		pgtest.Exec(t, `INSERT INTO integration.connector (tenant_id, id, kind, name, settings, secret_ref, status, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`, f.Tenant.ID, id, kind, name, settings, secret, status, f.Tenant.Human.ID)
		return id
	}
	valid := conn("bank_sftp", "Bank SFTP", settings, "env:BANK_KEY", "active")
	sap := conn("sap_file", "SAP", `{"path": "x.json"}`, "", "active")

	ep, err := endpointOf(t, f, b, valid)
	if err != nil {
		t.Fatal(err)
	}
	if want := "sftp.bank.example:2222"; ep.Addr != want {
		t.Errorf("address: got %s, want %s", ep.Addr, want)
	}
	if ep.User != "erp" || ep.Dir != "/in" {
		t.Errorf("user and directory: got %s %s, want erp /in", ep.User, ep.Dir)
	}
	if string(ep.HostKey.Marshal()) != string(host.Marshal()) {
		t.Error("host key: got another, want the pinned one")
	}
	if ep.Signer == nil || string(ep.Signer.PublicKey().Marshal()) != string(client.PublicKey().Marshal()) {
		t.Error("client key: got another, want the secret's")
	}

	for name, c := range map[string]struct {
		id   uuid.UUID
		code string
	}{
		"unknown":            {uuid.New(), "request.not_found"},
		"not a bank channel": {sap, "integration.not_a_bank_channel"},
		"disabled":           {conn("bank_sftp", "Off", settings, "env:BANK_KEY", "disabled"), "integration.connector_disabled"},
		"no secret_ref":      {conn("bank_sftp", "No key", settings, "", "active"), "integration.secret_required"},
		"invalid settings":   {conn("bank_sftp", "Old", `{"host": "h", "user": "u", "host_key": "x", "directory": "/"}`, "env:BANK_KEY", "active"), "integration.invalid_settings"},
		"secret missing":     {conn("bank_sftp", "Missing", settings, "env:MISSING", "active"), "integration.secret_unavailable"},
		"secret not a key":   {conn("bank_sftp", "Password", settings, "env:NOT_A_KEY", "active"), "integration.secret_unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := endpointOf(t, f, b, c.id); problem.From(err).Code != c.code {
				t.Errorf("got %v, want %s", err, c.code)
			}
		})
	}

	for name, want := range map[string]bool{"Bank SFTP": true, "SAP": false, "Nobody": false} {
		t.Run("by name "+name, func(t *testing.T) {
			var found bool
			if err := apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) (err error) {
				_, found, err = b.ChannelByName(ctx, name)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if found != want {
				t.Errorf("found: got %v, want %v", found, want)
			}
		})
	}

	t.Run("outside a unit of work and failing statements", func(t *testing.T) {
		for name, call := range map[string]func(ctx context.Context) error{
			"by name":  func(ctx context.Context) error { _, _, err := b.ChannelByName(ctx, "Bank SFTP"); return err },
			"endpoint": func(ctx context.Context) error { _, err := b.Endpoint(ctx, valid); return err },
		} {
			t.Run(name, func(t *testing.T) {
				if err := call(context.Background()); err == nil {
					t.Error("outside a unit of work: got nil error, want an error")
				}
				apitest.EveryFailure(t, f.Tenant, f.Tenant.Human, name, call)
			})
		}
	})
}

func endpointOf(t *testing.T, f fixture.Env, b integration.BankChannels, id uuid.UUID) (ep sftpclient.Endpoint, err error) {
	t.Helper()
	err = apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) (err error) {
		ep, err = b.Endpoint(ctx, id)
		return err
	})
	return ep, err
}
