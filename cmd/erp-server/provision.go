package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	platformconfig "github.com/mustie7iy/agentic-erp/internal/platform/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/provisioning"
)

// provisionCmd creates a tenant with its first administrator and its first
// configuration, on the server's database with the server's settings:
//
//	erp-server provision -name "Acme" -region eu-west -currency EUR \
//	  -admin-subject "auth0|abc" -admin-name "Ada" -config config.yaml
//
// It prints the ids as JSON. The administrator holds every permission; the
// roles the configuration names exist, empty, for the administrator to fill
// through the API.
func provisionCmd(ctx context.Context, args []string, cfg platformconfig.Config, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("erp-server provision", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "tenant name")
	region := fs.String("region", "", "region the tenant's data stays in, e.g. eu-west")
	currency := fs.String("currency", "", "group currency (ISO 4217)")
	subject := fs.String("admin-subject", "", "the administrator's subject at the identity provider")
	adminName := fs.String("admin-name", "", "the administrator's display name")
	email := fs.String("admin-email", "", "the administrator's email (optional)")
	file := fs.String("config", "", "the first configuration (YAML)")
	idpIssuer := fs.String("idp-issuer", "", "the tenant's identity provider's issuer (required with ERP_ENV=production)")
	idpAudience := fs.String("idp-audience", "", "the audience the provider issues the ERP's tokens for")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	req := provisioning.Request{Name: *name, Region: *region, GroupCurrency: *currency,
		Administrator: provisioning.Administrator{Subject: *subject, DisplayName: *adminName, Email: optional(*email)}, Operator: "erp-server provision"}
	if *idpIssuer != "" {
		req.IdentityProvider = &identity.NewIdentityProvider{Issuer: *idpIssuer, Audience: *idpAudience}
	}
	res, err := provision(ctx, cfg, req, *file)
	if err != nil {
		fmt.Fprintln(stderr, "erp-server provision:", err)
		return 1
	}
	// Ids, names and counts always encode.
	out, _ := json.MarshalIndent(map[string]any{"tenant_id": res.TenantID, "administrator_id": res.AdministratorID,
		"administrator_role": provisioning.AdministratorRole, "roles": res.Roles, "legal_entities": res.LegalEntities,
		"configuration_version": res.ConfigurationVersion, "identity_provider_id": res.IdentityProviderID}, "", "  ")
	fmt.Fprintln(stdout, string(out))
	return 0
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// provision reads the configuration file and provisions the tenant.
func provision(ctx context.Context, cfg platformconfig.Config, req provisioning.Request, file string) (provisioning.Result, error) {
	if file == "" {
		return provisioning.Result{}, errors.New("-config is required: the tenant's first configuration (erp config generate writes one)")
	}
	doc, err := config.Load(file)
	if err != nil {
		return provisioning.Result{}, err
	}
	req.Document = doc
	pool, err := database.Open(ctx, cfg.DatabaseURL, database.DefaultSettings)
	if err != nil {
		return provisioning.Result{}, err
	}
	defer pool.Close()
	keys, err := keyring(cfg, pool)
	if err != nil {
		return provisioning.Result{}, fmt.Errorf("provision: field keys: %w", err)
	}
	deps := app.Configuration(app.Config{FieldKeys: keys})
	production := cfg.Environment == platformconfig.Production
	p := &provisioning.Provisioner{Pool: pool, AppRole: cfg.AppRole, Keys: keys, Issuers: cfg.TenantIssuers(), Configuration: deps,
		Production: production, Discover: (&authn.Providers{Production: production}).Probe}
	// With a global directory the provider's issuer is claimed there before
	// the commit, as the control plane does (ADR-0031).
	if cfg.DirectoryURL != "" {
		dir, err := directory.Open(ctx, cfg.DirectoryURL)
		if err != nil {
			return provisioning.Result{}, err
		}
		defer dir.Close()
		p.Claim = (&directory.Sync{Directory: dir, Cell: cfg.Cell}).ClaimIssuer
	}
	return p.Provision(ctx, req)
}
