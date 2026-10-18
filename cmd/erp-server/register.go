package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/controlplane"
	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	platformconfig "github.com/mustie7iy/agentic-erp/internal/platform/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// registerProviderCmd registers a tenant's identity provider without the
// server, as the control plane's registerIdentityProvider does it (checked,
// probed, claimed in the global directory, audited and published, ADR-0031):
//
//	erp-server register-provider -tenant <id> -issuer https://idp.example.com -audience erp-api [-acr ...] [-amr mfa] [-certificate-bound]
//
// It is the remedy a production cell names when it refuses to start for an
// active tenant without a provider: the control plane is served by the
// cell it stopped. It prints the provider as JSON.
func registerProviderCmd(ctx context.Context, args []string, cfg platformconfig.Config, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("erp-server register-provider", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tenant := fs.String("tenant", "", "the tenant's id")
	issuer := fs.String("issuer", "", "the identity provider's issuer (iss)")
	audience := fs.String("audience", "", "the audience it issues the ERP's tokens for")
	acr := fs.String("acr", "", "acr values proving multi-factor, comma-separated (optional)")
	amr := fs.String("amr", "", "amr values proving multi-factor, comma-separated (optional)")
	bound := fs.Bool("certificate-bound", false, "the provider binds its tokens to the client's certificate (RFC 8705); production accepts agents' tokens only from such a provider")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	id, err := uuid.Parse(*tenant)
	if err != nil {
		fmt.Fprintln(stderr, "erp-server register-provider: -tenant must be the tenant's id")
		return 2
	}
	p, err := registerProvider(ctx, cfg, id, identity.NewIdentityProvider{Issuer: *issuer, Audience: *audience, ACRValues: list(*acr), AMRValues: list(*amr),
		CertificateBound: *bound})
	if err != nil {
		fmt.Fprintln(stderr, "erp-server register-provider:", err)
		return 1
	}
	out, _ := json.MarshalIndent(p, "", "  ") // ids, strings, lists and times always encode
	fmt.Fprintln(stdout, string(out))
	return 0
}

// list splits comma-separated values; nil for none.
func list(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// registerProvider registers the provider through the control plane's
// service, as an operator named for the command.
func registerProvider(ctx context.Context, cfg platformconfig.Config, tenant uuid.UUID, in identity.NewIdentityProvider) (controlplane.IdentityProvider, error) {
	pool, err := database.Open(ctx, cfg.DatabaseURL, database.DefaultSettings)
	if err != nil {
		return controlplane.IdentityProvider{}, err
	}
	defer pool.Close()
	production := cfg.Environment == platformconfig.Production
	s := &controlplane.Service{Pool: pool, AppRole: cfg.AppRole, ControlRole: cfg.ControlRole, Production: production,
		Discover: (&authn.Providers{Production: production}).Probe}
	if cfg.DirectoryURL != "" {
		dir, err := directory.OpenWith(ctx, cfg.DirectoryURL, cfg.DirectoryPassword)
		if err != nil {
			return controlplane.IdentityProvider{}, err
		}
		defer dir.Close()
		s.Directory = &directory.Sync{Directory: dir, Cell: cfg.Cell}
	}
	op := controlplane.OperatorFrom(reqctx.Claims{Issuer: "erp-server", Subject: "register-provider"})
	p, _, err := s.RegisterProvider(ctx, op, tenant, in, nil)
	return p, err
}
