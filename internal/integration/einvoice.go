package integration

import (
	"context"
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
// on loopback for tests and the walkthrough; the only adapter until the
// product owner names a provider (ADR-0061 Q1). Production refuses it.
const StandInAdapter = "standin"

// PINTAE is the only document format of an einvoice_provider connector in
// T65 (ADR-0061 §1, §3).
const PINTAE = "pint_ae"

// einvoiceKeys are the settings of an einvoice_provider connector.
var einvoiceKeys = []string{"adapter", "base_url", "participants", "format"}

// participantID is a company's participant identifier at the provider: its
// scheme is to confirm (ADR-0061 §3), so only its characters are checked.
var participantID = regexp.MustCompile(`^[A-Za-z0-9:._-]{1,100}$`)

// EInvoiceSettings are what valid einvoice_provider settings name.
type EInvoiceSettings struct {
	Adapter      string            // standin, the only one
	BaseURL      string            // https, without credentials, query or fragment
	Participants map[string]string // company code -> participant identifier
	Format       string            // pint_ae
}

// einvoiceSettings validates an einvoice_provider connector (ADR-0061 §3):
// its adapter, the provider's https base URL, the participant identifier
// of each company it receives for (at least one; each identifier once) and
// the format, pint_ae. The credentials are the secret_ref's.
func einvoiceSettings(settings map[string]any) (EInvoiceSettings, error) {
	for k := range settings {
		if !slices.Contains(einvoiceKeys, k) {
			return EInvoiceSettings{}, invalid("integration.invalid_settings", "settings."+k, "Not an einvoice_provider setting; credentials go in secret_ref.")
		}
	}
	var s EInvoiceSettings
	s.Adapter, _ = settings["adapter"].(string)  // not a string: "", refused below
	s.BaseURL, _ = settings["base_url"].(string) // as adapter
	s.Format, _ = settings["format"].(string)    // as adapter
	switch {
	case s.Adapter != StandInAdapter:
		return EInvoiceSettings{}, invalid("integration.invalid_settings", "settings.adapter", "The provider's adapter: standin, the stand-in, until a provider is chosen.")
	case !httpsBase(s.BaseURL):
		return EInvoiceSettings{}, invalid("integration.invalid_settings", "settings.base_url", "The provider's base URL: https, without credentials, query or fragment.")
	case s.Format != PINTAE:
		return EInvoiceSettings{}, invalid("integration.invalid_settings", "settings.format", "The documents' format: pint_ae, the only one.")
	}
	participants, ok := einvoiceParticipants(settings["participants"])
	if !ok {
		return EInvoiceSettings{}, invalid("integration.invalid_settings", "settings.participants",
			"Each company's participant identifier at the provider, by company code: at least one, each identifier once, 1-100 letters, digits or :._-.")
	}
	s.Participants = participants
	return s, nil
}

func httpsBase(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && !u.ForceQuery
}

// einvoiceParticipants reads participants: company code -> identifier,
// at least one, no identifier twice.
func einvoiceParticipants(raw any) (map[string]string, bool) {
	list, _ := raw.(map[string]any) // not an object: none, refused below
	out := make(map[string]string, len(list))
	seen := map[string]bool{}
	for code, v := range list {
		id, _ := v.(string) // not a string: "", refused below
		if !companyCode.MatchString(code) || !participantID.MatchString(id) || seen[id] {
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
		"The e-invoicing provider's stand-in is refused in production: no provider's adapter is certified yet (ADR-0061 Q1).")
}

// refuseUncertified refuses, in production, a connector becoming active
// that is not certified there: a bank_ebics channel (ADR-0038) or an
// einvoice_provider of the stand-in's adapter (ADR-0061 §3).
func (e *Endpoints) refuseUncertified(kind string, settings map[string]any) error {
	if !e.RefuseEBICS {
		return nil
	}
	switch {
	case kind == BankEBICS:
		return ebicsRefused()
	case kind == EInvoiceProvider && settings["adapter"] == StandInAdapter:
		return standInRefused()
	}
	return nil
}

// EInvoiceProviders are the einvoice_provider connectors, published to the
// einvoicing context (ADR-0061 §2), which reaches them through a port the
// application wires, so neither context imports the other.
type EInvoiceProviders struct {
	Secrets secrets.Resolver // resolves a connector's credentials
	// RefuseStandIn refuses the stand-in's adapter at use (production): a
	// connector stored before, or changed since, that the API would refuse.
	RefuseStandIn bool
}

// EInvoiceConnector is an einvoice_provider connector resolved for use:
// its settings and its credentials, resolved now.
type EInvoiceConnector struct {
	ID   uuid.UUID
	Name string
	EInvoiceSettings
	Secret string
}

// Provider resolves an einvoice_provider connector for a fetch, failing
// closed: a connector that is not one, is disabled, names no secret_ref,
// has settings that do not resolve, is the stand-in in production, or
// whose credentials do not resolve is refused, and nothing is fetched. The
// credentials are resolved on each use, so a rotated secret takes effect
// without a restart.
func (p EInvoiceProviders) Provider(ctx context.Context, id uuid.UUID) (EInvoiceConnector, error) {
	tx, _, err := reqctx.Request(ctx)
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
		return EInvoiceConnector{}, problem.Conflict("integration.connector_disabled", "The e-invoicing provider "+c.Name+" is disabled; enable it to fetch.")
	case c.SecretRef == nil:
		return EInvoiceConnector{}, problem.Unprocessable("integration.secret_required",
			"Connector "+c.Name+" names no secret_ref: the provider's credentials are a secret.")
	}
	s, err := einvoiceSettings(c.Settings) // a connector changed by hand is refused here
	if err != nil {
		return EInvoiceConnector{}, err
	}
	if p.RefuseStandIn && s.Adapter == StandInAdapter {
		return EInvoiceConnector{}, standInRefused()
	}
	secret, err := p.Secrets.Resolve(ctx, *c.SecretRef)
	if err != nil {
		return EInvoiceConnector{}, problem.Unprocessable("integration.secret_unavailable",
			"The e-invoicing provider's credentials ("+*c.SecretRef+") cannot be resolved.").WithCause(err)
	}
	return EInvoiceConnector{ID: c.ID, Name: c.Name, EInvoiceSettings: s, Secret: secret}, nil
}

// Names gives the names of connectors by id, those the tenant has.
func (EInvoiceProviders) Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	return connectorNames(tx)(ctx, ids)
}
