package ledger

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// PaymentReleases tells the ledger which payment runs are being released
// with payments not posted yet (the payments context; ADR-0038 As built,
// review batch 77). A release posts its payments on the run date one
// transaction at a time, and one stopped with some posted stays releasing
// until it is resumed; closing or locking the run date's period in
// between would refuse the rest for good, with the bank file of the
// posted ones never written.
type PaymentReleases interface {
	// Releasing returns the legal entity's runs being released, or stopped
	// and waiting to be resumed, with a payment not posted yet dated from
	// start to end, by document number.
	Releasing(ctx context.Context, legalEntity uuid.UUID, start, end time.Time) ([]ReleasingRun, error)
}

// ReleasingRun is a payment run being released: its id and number.
type ReleasingRun struct {
	ID             uuid.UUID
	DocumentNumber string
}

// errNoPaymentReleases is a ledger wired without the payments context,
// which must not close a period as though no release were under way.
var errNoPaymentReleases = errors.New("ledger: no payment releases port to check a period close against")

// noRelease refuses to close or lock regular period p of a company while a
// payment run of the company is releasing with payments not posted yet
// dated in it (409 ledger.payment_release_pending), naming the runs by
// number and id: the release is resumed, and finishes, first. The caller
// holds the period's row FOR UPDATE, which a posting takes FOR SHARE
// (postingPeriod): a payment posting under way finishes before the runs
// are read here, and a release that started after they were read finds
// the period closed at its first posting and, nothing posted, goes back
// to proposed. A special period takes no payment: a run posts in its run
// date's regular period.
func noRelease(ctx context.Context, releases PaymentReleases, p Period, to string) error {
	if p.IsSpecial {
		return nil
	}
	if releases == nil {
		return errNoPaymentReleases
	}
	runs, err := releases.Releasing(ctx, p.LegalEntityID, p.StartDate, p.EndDate)
	if err != nil || len(runs) == 0 {
		return err
	}
	named := make([]string, len(runs))
	for i, r := range runs {
		named[i] = r.DocumentNumber + " (" + r.ID.String() + ")"
	}
	which := "payment run " + named[0] + " of the company is"
	if len(runs) > 1 {
		which = "payment runs " + strings.Join(named, ", ") + " of the company are"
	}
	return problem.Conflict("ledger.payment_release_pending", "The period cannot be "+to+" while "+which+" being released with payments "+
		"dated in it not posted yet: resume the release (:release) and let it finish first.")
}
