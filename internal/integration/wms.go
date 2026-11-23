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

// WMS is the kind of a warehouse management system's connector.
const WMS = "wms"

// wmsKeys are the settings of a wms connector: the adapter, the stand-in's
// base URL and the plants it manages. Credentials are the secret_ref's.
var wmsKeys = []string{"adapter", "base_url", "plants"}

// WMSSettings are what valid wms settings name.
type WMSSettings struct {
	Adapter string              // standin
	BaseURL string              // https, without credentials, query or fragment
	Plants  map[string][]string // company code -> its plants' codes, each sorted, at least one
}

// wmsSettings validates a wms connector's settings: the stand-in's
// adapter and its https base URL, and the plants the WMS manages by
// company code, at least one plant, each once.
func wmsSettings(settings map[string]any) (WMSSettings, error) {
	for _, k := range slices.Sorted(maps.Keys(settings)) { // the first unknown key by name, whatever the map's order
		if !slices.Contains(wmsKeys, k) {
			return WMSSettings{}, problem.Invalid("integration.invalid_settings", "settings."+k, "Not a wms setting; credentials go in secret_ref.")
		}
	}
	var s WMSSettings
	s.Adapter, _ = settings["adapter"].(string)  // not a string: "", refused below
	s.BaseURL, _ = settings["base_url"].(string) // as adapter
	switch {
	case s.Adapter != StandInAdapter:
		return WMSSettings{}, problem.Invalid("integration.invalid_settings", "settings.adapter",
			"The warehouse system's adapter: standin (the stand-in), the only one until a WMS vendor is chosen.")
	case !httpsBase(s.BaseURL):
		return WMSSettings{}, problem.Invalid("integration.invalid_settings", "settings.base_url",
			"The warehouse system's base URL: https, without credentials, query or fragment.")
	}
	plants, ok := wmsPlants(settings["plants"])
	if !ok {
		return WMSSettings{}, problem.Invalid("integration.invalid_settings", "settings.plants",
			"The plants the warehouse system manages, by company code: a list of plant codes for each company, at least one plant, each once.")
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

// wmsStandInRefused is the refusal of the WMS's stand-in in production.
func wmsStandInRefused() error {
	return problem.Unprocessable("integration.adapter_not_certified",
		"The warehouse system's stand-in is refused in production: no WMS vendor's adapter is built yet.")
}

// wmsLock serializes, per tenant, the writes that make a wms connector
// active with its plants, so two cannot both take one plant (no check
// before the lock that guards it).
func wmsLock(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('integration.wms:' || platform.require_tenant()::text, 0))`); err != nil {
		return fmt.Errorf("integration: lock the tenant's wms connectors: %w", err)
	}
	return nil
}

// plantsFree refuses settings naming a plant another active wms connector
// manages (409 integration.plant_has_wms), under wmsLock; self is the
// connector itself, which may keep its plants.
func plantsFree(ctx context.Context, tx pgx.Tx, self uuid.UUID, s WMSSettings) error {
	if err := wmsLock(ctx, tx); err != nil {
		return err
	}
	others, err := activeWMS(ctx, tx)
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
					return problem.Conflict("integration.plant_has_wms",
						fmt.Sprintf("Plant %s of company %s is managed by the warehouse system of connector %s already.", plant, company, o.Name))
				}
			}
		}
	}
	return nil
}

// wmsConnector is an active wms connector with its settings read.
type wmsConnector struct {
	ID        uuid.UUID
	Name      string
	SecretRef *string
	WMSSettings
}

// activeWMS reads the tenant's active wms connectors, by name. One whose
// settings no longer resolve (changed by hand) manages nothing.
func activeWMS(ctx context.Context, tx pgx.Tx) ([]wmsConnector, error) {
	rows, _ := tx.Query(ctx, `SELECT `+connectorColumns+` FROM integration.connector WHERE kind = $1 AND status = 'active' ORDER BY name, id`, WMS)
	cs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Connector, error) { return scanConnector(r) })
	if err != nil {
		return nil, fmt.Errorf("integration: read the active wms connectors: %w", err)
	}
	out := make([]wmsConnector, 0, len(cs))
	for _, c := range cs {
		if s, err := wmsSettings(c.Settings); err == nil {
			out = append(out, wmsConnector{ID: c.ID, Name: c.Name, SecretRef: c.SecretRef, WMSSettings: s})
		}
	}
	return out, nil
}

// wmsOf is the active wms connector managing the company's plant, if any.
func wmsOf(ctx context.Context, tx pgx.Tx, company, plant string) (wmsConnector, bool, error) {
	cs, err := activeWMS(ctx, tx)
	if err != nil {
		return wmsConnector{}, false, err
	}
	for _, c := range cs {
		if c.Manages(company, plant) {
			return c, true, nil
		}
	}
	return wmsConnector{}, false, nil
}

// checkWMSActive refuses a wms connector becoming, or staying, active
// with plants another active one manages; nil for any other kind or for
// a connector that will not be active.
func checkWMSActive(ctx context.Context, tx pgx.Tx, id uuid.UUID, kind, status string, settings map[string]any) error {
	if kind != WMS || status != "active" {
		return nil
	}
	s, err := wmsSettings(settings)
	if err != nil {
		return err
	}
	return plantsFree(ctx, tx, id, s)
}
