package integration

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// The WMS connector (S2q1; ADR-0082 As built S2q): an external warehouse
// management system a tenant connects for some of its plants. The ERP
// sends it each outbound delivery created at such a plant to pick
// (wms_queue.go, wms_send.go) and posts the goods issues it reports back,
// once per message id (wms_receive.go). The connector names the plants
// it manages; a plant is managed by one active wms connector at most, so
// a delivery goes to one warehouse system and a goods issue is reported
// by the one it was sent to. The WMS is reached through a
// provider-neutral port (Warehouse), its adapter named by the settings:
// the stand-in's alone until a vendor is chosen, refused in production.
//
// The TMS connector (S2r; ADR-0082 As built S2r) is the same for a
// transport management system: it names the plants it plans for, one
// active tms connector a plant (beside its wms one, if any), is sent each
// delivery created there to plan and reports the shipment back, recorded
// on the delivery (tms.go). Both kinds share these settings, the port,
// the queue, the exchange and integration.wms_message, a message kind
// each way, as S2q2's receipts do.

// WMS is the kind of a warehouse management system's connector.
const WMS = "wms"

// TMS is the kind of a transport management system's connector (S2r).
const TMS = "tms"

// systemName is what the kind's connector reaches, as problems name it.
func systemName(kind string) string {
	if kind == TMS {
		return "transport system"
	}
	return "warehouse system"
}

// wmsKeys are the settings of a wms or tms connector: the adapter, the
// stand-in's base URL and the plants it manages. Credentials are the
// secret_ref's.
var wmsKeys = []string{"adapter", "base_url", "plants"}

// WMSSettings are what valid wms or tms settings name.
type WMSSettings struct {
	Adapter string              // standin
	BaseURL string              // https, without credentials, query or fragment
	Plants  map[string][]string // company code -> its plants' codes, each sorted, at least one
}

// plantSettings validates a wms or tms connector's settings: the
// stand-in's adapter and its https base URL, and the plants the system
// manages by company code, at least one plant, each once.
func plantSettings(kind string, settings map[string]any) (WMSSettings, error) {
	system := systemName(kind)
	for _, k := range slices.Sorted(maps.Keys(settings)) { // the first unknown key by name, whatever the map's order
		if !slices.Contains(wmsKeys, k) {
			return WMSSettings{}, problem.Invalid("integration.invalid_settings", "settings."+k, "Not a "+kind+" setting; credentials go in secret_ref.")
		}
	}
	var s WMSSettings
	s.Adapter, _ = settings["adapter"].(string)  // not a string: "", refused below
	s.BaseURL, _ = settings["base_url"].(string) // as adapter
	switch {
	case s.Adapter != StandInAdapter:
		return WMSSettings{}, problem.Invalid("integration.invalid_settings", "settings.adapter",
			"The "+system+"'s adapter: standin (the stand-in), the only one until a vendor is chosen.")
	case !httpsBase(s.BaseURL):
		return WMSSettings{}, problem.Invalid("integration.invalid_settings", "settings.base_url",
			"The "+system+"'s base URL: https, without credentials, query or fragment.")
	}
	plants, ok := wmsPlants(settings["plants"])
	if !ok {
		return WMSSettings{}, problem.Invalid("integration.invalid_settings", "settings.plants",
			"The plants the "+system+" manages, by company code: a list of plant codes for each company, at least one plant, each once.")
	}
	s.Plants = plants
	return s, nil
}

// wmsPlants reads plants: company code -> plant codes, at least one, none
// twice within a company.
func wmsPlants(raw any) (map[string][]string, bool) {
	byCompany, _ := raw.(map[string]any) // not an object: none, refused below
	out := make(map[string][]string, len(byCompany))
	for code, v := range byCompany {
		list, _ := v.([]any) // not a list: none, refused below
		if !companyCode.MatchString(code) || len(list) == 0 {
			return nil, false
		}
		var plants []string
		for _, p := range list {
			plant, _ := p.(string) // not a string: "", refused below
			if !orgUnitCode.MatchString(plant) || slices.Contains(plants, plant) {
				return nil, false
			}
			plants = append(plants, plant)
		}
		sort.Strings(plants)
		out[code] = plants
	}
	return out, len(out) > 0
}

// Manages reports whether the settings name the company's plant.
func (s WMSSettings) Manages(company, plant string) bool {
	return slices.Contains(s.Plants[company], plant)
}

// logisticsStandInRefused is the refusal of the kind's stand-in in production.
func logisticsStandInRefused(kind string) error {
	return problem.Unprocessable("integration.adapter_not_certified",
		"The "+systemName(kind)+"'s stand-in is refused in production: no vendor's adapter is built yet.")
}

// plantLock serializes, per tenant and kind, the writes that make a wms
// or tms connector active with its plants, so two cannot both take one
// plant (no check before the lock that guards it).
func plantLock(ctx context.Context, tx pgx.Tx, kind string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('integration.' || $1 || ':' || platform.require_tenant()::text, 0))`,
		kind); err != nil {
		return fmt.Errorf("integration: lock the tenant's %s connectors: %w", kind, err)
	}
	return nil
}

// plantTaken refuses a plant another active connector of the kind manages
// already: 409 integration.plant_has_wms, or integration.plant_has_tms.
func plantTaken(kind, plant, company, other string) error {
	detail := fmt.Sprintf("Plant %s of company %s is managed by the %s of connector %s already.", plant, company, systemName(kind), other)
	if kind == TMS {
		return problem.Conflict("integration.plant_has_tms", detail)
	}
	return problem.Conflict("integration.plant_has_wms", detail)
}

// plantsFree refuses settings naming a plant another active connector of
// the kind manages, under plantLock; self is the connector itself, which
// may keep its plants.
func plantsFree(ctx context.Context, tx pgx.Tx, self uuid.UUID, kind string, s WMSSettings) error {
	if err := plantLock(ctx, tx, kind); err != nil {
		return err
	}
	others, err := activeLogistics(ctx, tx, kind)
	if err != nil {
		return err
	}
	for _, o := range others {
		if o.ID == self {
			continue
		}
		for _, company := range slices.Sorted(maps.Keys(s.Plants)) { // the first taken plant by company and code
			for _, plant := range s.Plants[company] {
				if o.Manages(company, plant) {
					return plantTaken(kind, plant, company, o.Name)
				}
			}
		}
	}
	return nil
}

// logisticsConnector is an active wms or tms connector with its settings read.
type logisticsConnector struct {
	ID        uuid.UUID
	Kind      string
	Name      string
	SecretRef *string
	WMSSettings
}

// activeLogistics reads the tenant's active connectors of the kinds, by
// name. One whose settings no longer resolve (changed by hand) manages
// nothing.
func activeLogistics(ctx context.Context, tx pgx.Tx, kinds ...string) ([]logisticsConnector, error) {
	rows, _ := tx.Query(ctx, `SELECT `+connectorColumns+` FROM integration.connector WHERE kind = ANY($1) AND status = 'active' ORDER BY name, id`, kinds)
	cs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Connector, error) { return scanConnector(r) })
	if err != nil {
		return nil, fmt.Errorf("integration: read the active %v connectors: %w", kinds, err)
	}
	out := make([]logisticsConnector, 0, len(cs))
	for _, c := range cs {
		if s, err := plantSettings(c.Kind, c.Settings); err == nil {
			out = append(out, logisticsConnector{ID: c.ID, Kind: c.Kind, Name: c.Name, SecretRef: c.SecretRef, WMSSettings: s})
		}
	}
	return out, nil
}

// managing is the active connector of the kind managing the company's
// plant, if any.
func managing(ctx context.Context, tx pgx.Tx, kind, company, plant string) (logisticsConnector, bool, error) {
	cs, err := activeLogistics(ctx, tx, kind)
	if err != nil {
		return logisticsConnector{}, false, err
	}
	for _, c := range cs {
		if c.Manages(company, plant) {
			return c, true, nil
		}
	}
	return logisticsConnector{}, false, nil
}

// checkPlantsActive refuses a wms or tms connector becoming, or staying,
// active with plants another active one of its kind manages; nil for any
// other kind or for a connector that will not be active.
func checkPlantsActive(ctx context.Context, tx pgx.Tx, id uuid.UUID, kind, status string, settings map[string]any) error {
	if (kind != WMS && kind != TMS) || status != "active" {
		return nil
	}
	s, err := plantSettings(kind, settings)
	if err != nil {
		return err
	}
	return plantsFree(ctx, tx, id, kind, s)
}
