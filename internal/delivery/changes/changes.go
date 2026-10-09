// Package changes runs the checks a configuration change request needs
// before people approve it: a sandbox replay of recent transactions and the
// regression suite, both attached to the request as evidence.
package changes

import (
	"fmt"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/delivery/regress"
	"github.com/mustie7iy/agentic-erp/internal/delivery/simulate"
)

// AttachEvidence simulates a change request against the live data (in
// sandboxes) and runs the suite, then records both results on the request.
// The suite must still describe the active configuration; a suite whose
// own baseline fails is out of date and is refused.
func AttachEvidence(repo *config.Repo, crID string, live *core.Store, suite *regress.Suite, from, to string, now time.Time) (*config.ChangeRequest, error) {
	cr, err := repo.Get(crID)
	if err != nil {
		return nil, err
	}
	active, _, err := repo.Active()
	if err != nil {
		return nil, err
	}
	proposed, err := config.Parse([]byte(cr.Proposed))
	if err != nil {
		return nil, err
	}
	if config.HasErrors(config.Validate(proposed)) {
		return nil, fmt.Errorf("change request %s has validation errors; fix them before simulating", crID)
	}
	report, err := simulate.Replay(live, active, proposed, from, to, now.Format(time.DateOnly))
	if err != nil {
		return nil, err
	}
	baseline, err := suite.Run(active)
	if err != nil {
		return nil, err
	}
	if !baseline.Passed() {
		return nil, fmt.Errorf("regression suite %s no longer matches the active configuration (%d cases differ); capture a new suite", suite.ID, len(baseline.Failed))
	}
	result, err := suite.Run(proposed)
	if err != nil {
		return nil, err
	}
	if _, err := repo.AddEvidence(crID, report.Evidence()); err != nil {
		return nil, err
	}
	return repo.AddEvidence(crID, result.Evidence())
}
