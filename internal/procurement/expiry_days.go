package procurement

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// companyDay is a legal entity's date at the round, in its company's time
// zone, and the zone's name, which the events and audit entries give
// beside the date so a consumer knows which day judged the contract; and
// its company code, which the events name the company by (eventNames), so
// a round reads each legal entity once (ADR-0034 as built).
type companyDay struct {
	date time.Time
	zone string
	code string
}

// companyDays are the round's dates by legal entity.
type companyDays map[uuid.UUID]companyDay

// args are the legal entities and their dates as parallel arrays, for a
// statement to join the contracts with.
func (d companyDays) args() ([]uuid.UUID, []time.Time) {
	entities, dates := make([]uuid.UUID, 0, len(d)), make([]time.Time, 0, len(d))
	for id, day := range d {
		entities, dates = append(entities, id), append(dates, day.date)
	}
	return entities, dates
}

// document is the tenant's active configuration; ok is false when the
// tenant has none yet, whose contracts are judged on UTC's date and none
// alerted, as before a configuration existed.
func (x *ContractExpirer) document(ctx context.Context) (doc *config.Config, ok bool, err error) {
	doc, _, err = x.Settings.Document(ctx)
	if err != nil {
		if problem.From(err).Code == "config.not_bootstrapped" {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("procurement: read the configuration for contract expiry: %w", err)
	}
	return doc, true, nil
}

// companyDays dates the round for every legal entity holding active
// contracts: the round's instant in its company's zone
// (config.EntityDay: UTC when the company sets none, or the tenant has no
// configuration). A stored zone the binary cannot load, or a legal entity
// the directory does not know, fails the round rather than judging the
// company's contracts in UTC. Each legal entity's company code is kept for
// its events. The read takes no lock: the statements that
// pick contracts join these dates and lock and check each contract's
// status themselves, so the dates only narrow what they pick, and a
// contract activated in a further legal entity after this read is judged
// next round.
func (x *ContractExpirer) companyDays(ctx context.Context, tx pgx.Tx, doc *config.Config, now time.Time) (companyDays, error) {
	rows, _ := tx.Query(ctx, `SELECT DISTINCT legal_entity_id FROM procurement.contract WHERE status = 'active'`)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("procurement: list the legal entities with active contracts: %w", err)
	}
	if doc == nil {
		// No configuration yet: no company sets a zone, so every date is
		// UTC's, as config.CompanyDay gives it for a company the document
		// does not have.
		doc = &config.Config{}
	}
	read, days := tenancy.CompanyCode(x.LegalEntities), companyDays{}
	for _, id := range ids {
		var code string
		keep := func(ctx context.Context, le uuid.UUID) (string, bool, error) {
			var found bool
			var err error
			code, found, err = read(ctx, le)
			return code, found, err
		}
		d, err := doc.EntityDay(ctx, keep, id, now)
		if err != nil {
			return nil, fmt.Errorf("procurement: %w", err)
		}
		days[id] = companyDay{date: d.Date, zone: d.Zone.String(), code: code}
	}
	return days, nil
}
