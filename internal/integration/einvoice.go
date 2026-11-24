package integration

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
)

// EInvoiceProvider is the kind of a tenant's accredited e-invoicing
// provider (ADR-0061 §3): the access point the einvoicing context fetches
// its companies' received e-invoices from, through the adapter its
// settings name.
const EInvoiceProvider = "einvoice_provider"

// StandInAdapter is the adapter of the provider's stand-in, an HTTP server
// on loopback for tests and the walkthrough. Production refuses it.
const StandInAdapter = "standin"

// EvatraAdapter is the adapter of Evatra (Hamt Information Technology
// L.L.C, the Ministry of Finance's accredited service provider 144576), the
// first provider's (ADR-0061 §3, Q1 answered: po:2026-11-19). Its base URL
// is not a setting: the einvoicing context derives it from the
// environment, one of Evatra's two hosts.
const EvatraAdapter = "evatra"

// Evatra's environments: its sandbox (stage) and production (prod).
// Production refuses stage, whose documents are not statutory ones.
const (
	EvatraStage = "stage"
	EvatraProd  = "prod"
)

// PINTAE is the only document format of an einvoice_provider connector in
// T65 (ADR-0061 §1, §3).
const PINTAE = "pint_ae"

// einvoiceKeys are the settings of an einvoice_provider connector, by
// adapter.
var einvoiceKeys = map[string][]string{
	StandInAdapter: {"adapter", "base_url", "participants", "format"},
	EvatraAdapter:  {"adapter", "environment", "company_id", "participants", "format"},
}

// participantID is a company's participant identifier at the stand-in: only
// its characters are checked.
var participantID = regexp.MustCompile(`^[A-Za-z0-9:._-]{1,100}$`)

// peppolAE is a company's participant identifier at Evatra: the scheme 0235
// (businesses registered in the UAE) and the company's TIN, the first 10
// digits of its 15-digit TRN (ADR-0061 §5 C6, the UAE tax specialist,
// po:2026-11-09).
var peppolAE = regexp.MustCompile(`^0235:[0-9]{10}$`)

// evatraCompany is the company's id at Evatra (compId): its form is not
// documented, so only its characters are checked.
var evatraCompany = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// EInvoiceSettings are what valid einvoice_provider settings name.
type EInvoiceSettings struct {
	Adapter      string            // standin or evatra
	BaseURL      string            // the stand-in's: https, without credentials, query or fragment
	Environment  string            // Evatra's: stage or prod
	CompanyID    string            // Evatra's: the company's id at Evatra
	Participants map[string]string // company code -> participant identifier
	Format       string            // pint_ae
}

// einvoiceSettings validates an einvoice_provider connector (ADR-0061 §3):
// its adapter; the stand-in's https base URL, or Evatra's environment and
// company id; the participant identifier of each company it receives for
// (at least one; each identifier once; 0235:<TIN> at Evatra) and the
// format, pint_ae. The credentials are the secret_ref's.
func einvoiceSettings(settings map[string]any) (EInvoiceSettings, error) {
	var s EInvoiceSettings
	s.Adapter, _ = settings["adapter"].(string) // not a string: "", refused below
	keys, ok := einvoiceKeys[s.Adapter]
	if !ok {
		return EInvoiceSettings{}, problem.Invalid("integration.invalid_settings", "settings.adapter", "The provider's adapter: evatra (Evatra) or standin (the stand-in).")
	}
	for k := range settings {
		if !slices.Contains(keys, k) {
			return EInvoiceSettings{}, problem.Invalid("integration.invalid_settings", "settings."+k, "Not a setting of the "+s.Adapter+" adapter; credentials go in secret_ref.")
		}
	}
	s.Format, _ = settings["format"].(string) // as adapter
	if err := adapterSettings(&s, settings); err != nil {
		return EInvoiceSettings{}, err
	}
	if s.Format != PINTAE {
		return EInvoiceSettings{}, problem.Invalid("integration.invalid_settings", "settings.format", "The documents' format: pint_ae, the only one.")
	}
	participants, ok := einvoiceParticipants(settings["participants"], s.Adapter)
	if !ok {
		return EInvoiceSettings{}, problem.Invalid("integration.invalid_settings", "settings.participants", participantsRule[s.Adapter])
	}
	s.Participants = participants
	return s, nil
}

// participantsRule says, per adapter, what the participants must be.
var participantsRule = map[string]string{
	StandInAdapter: "Each company's participant identifier at the provider, by company code: at least one, each identifier once, 1-100 letters, digits or :._-.",
	EvatraAdapter:  "Each company's Peppol participant identifier, by company code: 0235: and the company's TIN, the first 10 digits of its TRN; at least one, each once.",
}

// adapterSettings reads the adapter's own settings: the stand-in's base URL,
// or Evatra's environment and company id.
func adapterSettings(s *EInvoiceSettings, settings map[string]any) error {
	if s.Adapter == StandInAdapter {
		s.BaseURL, _ = settings["base_url"].(string) // not a string: "", refused below
		if !httpsBase(s.BaseURL) {
			return problem.Invalid("integration.invalid_settings", "settings.base_url", "The provider's base URL: https, without credentials, query or fragment.")
		}
		return nil
	}
	s.Environment, _ = settings["environment"].(string) // as base_url
	s.CompanyID, _ = settings["company_id"].(string)    // as base_url
	switch {
	case s.Environment != EvatraStage && s.Environment != EvatraProd:
		return problem.Invalid("integration.invalid_settings", "settings.environment", "Evatra's environment: stage (its sandbox) or prod.")
	case !evatraCompany.MatchString(s.CompanyID):
		return problem.Invalid("integration.invalid_settings", "settings.company_id", "The company's id at Evatra: 1-100 letters, digits or ._-.")
	}
	return nil
}

func httpsBase(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && !u.ForceQuery
}

// einvoiceParticipants reads participants: company code -> identifier of
// the adapter's form, at least one, no identifier twice.
func einvoiceParticipants(raw any, adapter string) (map[string]string, bool) {
	form := participantID
	if adapter == EvatraAdapter {
		form = peppolAE
	}
	list, _ := raw.(map[string]any) // not an object: none, refused below
	out := make(map[string]string, len(list))
	seen := map[string]bool{}
	for code, v := range list {
		id, _ := v.(string) // not a string: "", refused below
		if !companyCode.MatchString(code) || !form.MatchString(id) || seen[id] {
			return nil, false
		}
		seen[id] = true
		out[code] = id
	}
	return out, len(out) > 0
}

// standInRefused is the refusal of the provider's stand-in in production.
func standInRefused() error {
	return problem.Unprocessable("integration.adapter_not_certified",
		"The e-invoicing provider's stand-in is refused in production: connect Evatra (adapter evatra, environment prod).")
}

// stageRefused is the refusal of Evatra's sandbox in production: what it
// delivers is not a statutory document.
func stageRefused() error {
	return problem.Unprocessable("integration.adapter_not_certified",
		"Evatra's stage environment is its sandbox and is refused in production: use environment prod.")
}

// uncertifiedEInvoice is the refusal in production of an einvoice_provider
// connector's settings: the stand-in, or Evatra's sandbox; nil otherwise.
func uncertifiedEInvoice(adapter, environment any) error {
	switch {
	case adapter == StandInAdapter:
		return standInRefused()
	case adapter == EvatraAdapter && environment == EvatraStage:
		return stageRefused()
	}
	return nil
}

// refuseUncertified refuses, in production, a connector becoming active
// that is not certified there: a bank_ebics channel (ADR-0038), an
// einvoice_provider of the stand-in's adapter or of Evatra's sandbox
// (ADR-0061 §3), or a wms, tms or gts connector of the stand-in's (ADR-0082
// As built S2q, S2r, S2s).
func (e *Endpoints) refuseUncertified(kind string, settings map[string]any) error {
	if !e.RefuseEBICS {
		return nil
	}
	switch kind {
	case BankEBICS:
		return ebicsRefused()
	case EInvoiceProvider:
		return uncertifiedEInvoice(settings["adapter"], settings["environment"])
	case WMS, TMS, GTS:
		if settings["adapter"] == StandInAdapter {
			return logisticsStandInRefused(kind)
		}
	}
	return nil
}

// EInvoiceProviders are the einvoice_provider connectors, published to the
// einvoicing context (ADR-0061 §2), which reaches them through a port the
// application wires, so neither context imports the other.
type EInvoiceProviders struct {
	Secrets secrets.Resolver // resolves a connector's credentials
	// RefuseStandIn refuses the stand-in's adapter and Evatra's sandbox at
	// use (production): a connector stored before, or changed since, that
	// the API would refuse.
	RefuseStandIn bool
}

// EInvoiceConnector is an einvoice_provider connector read for use: its
// tenant, settings and secret_ref, and, once Credentials resolved them,
// its credentials.
type EInvoiceConnector struct {
	ID     uuid.UUID
	Tenant uuid.UUID
	Name   string
	EInvoiceSettings
	SecretRef string
	Secret    string
}

// Provider reads an einvoice_provider connector for use in the caller's
// transaction, failing closed: a connector that is not one, is disabled,
// names no secret_ref, has settings that do not resolve, or is the
// stand-in or Evatra's sandbox in production is refused. Its credentials
// are not resolved here: the vault is called by Credentials, with no
// transaction open (ADR-0061 As built E3).
func (p EInvoiceProviders) Provider(ctx context.Context, id uuid.UUID) (EInvoiceConnector, error) {
	tx, principal, err := reqctx.Request(ctx)
	if err != nil {
		return EInvoiceConnector{}, err
	}
	c, err := getConnector(ctx, tx, id, false)
	if err != nil {
		return EInvoiceConnector{}, err
	}
	switch {
	case c.Kind != EInvoiceProvider:
		return EInvoiceConnector{}, problem.Unprocessable("integration.not_an_einvoice_provider",
			"Connector "+c.Name+" is a "+c.Kind+" connector, not an einvoice_provider.")
	case c.Status != "active":
		return EInvoiceConnector{}, problem.Conflict("integration.connector_disabled", "The e-invoicing provider "+c.Name+" is disabled; enable it to use it.")
	case c.SecretRef == nil:
		return EInvoiceConnector{}, problem.Unprocessable("integration.secret_required",
			"Connector "+c.Name+" names no secret_ref: the provider's credentials are a secret.")
	}
	s, err := einvoiceSettings(c.Settings) // a connector changed by hand is refused here
	if err != nil {
		return EInvoiceConnector{}, err
	}
	if p.RefuseStandIn {
		if err := uncertifiedEInvoice(s.Adapter, s.Environment); err != nil {
			return EInvoiceConnector{}, err
		}
	}
	return EInvoiceConnector{ID: c.ID, Tenant: principal.TenantID, Name: c.Name, EInvoiceSettings: s, SecretRef: *c.SecretRef}, nil
}

// Credentials resolves the credentials of a connector Provider read, under
// its tenant's own namespace, failing closed. It opens no transaction and
// is called after the one that read the connector committed, so no
// transaction stays open while the vault is called; they are resolved on
// each use, so a rotated secret takes effect without a restart. A
// reference that names nothing, or is not the tenant's, is refused (422
// integration.secret_unavailable), which another try would meet again;
// any other failure (the vault unreachable or failing) is an error to try
// again, so a step is not failed for good by a passing outage.
func (p EInvoiceProviders) Credentials(ctx context.Context, c EInvoiceConnector) (string, error) {
	secret, err := tenantSecret(ctx, p.Secrets, c.Tenant, c.SecretRef)
	switch {
	case errors.Is(err, secrets.ErrNotFound) || errors.Is(err, secrets.ErrNotTenants):
		return "", problem.Unprocessable("integration.secret_unavailable",
			"The e-invoicing provider's credentials ("+c.SecretRef+") cannot be resolved.").WithCause(err)
	case err != nil:
		return "", fmt.Errorf("integration: the credentials of connector %s: %w", c.Name, err)
	}
	return secret, nil
}

// Names gives the names of connectors by id, those the tenant has.
func (EInvoiceProviders) Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	return connectorNames(tx)(ctx, ids)
}
