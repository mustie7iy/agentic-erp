package workflows

import (
	"context"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/mustie7iy/agentic-erp/internal/configuration"
)

// Configuration simulations (T28, ADR-0012): a tier 3 or 4 change request's
// simulation and regression evidence is one unit of work, the simulation
// worker's, run as the activity of a workflow named after the request.

// SimulateInput names the simulation operation a workflow runs.
type SimulateInput struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	OperationID uuid.UUID `json:"operation_id"`
}

// SimulateWorkflow is the registered name of SimulateChange.
const SimulateWorkflow = "SimulateChange"

// SimulateChange runs the simulation in one activity, retried on failure;
// an operation no longer queued (done, or cancelled) is left alone.
func SimulateChange(ctx workflow.Context, in SimulateInput) error {
	ao := workflow.ActivityOptions{StartToCloseTimeout: 30 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: time.Minute, MaximumAttempts: 5}}
	var a *Simulations
	return workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, ao), a.Simulate, in).Get(ctx, nil) //nolint:wrapcheck // workflow result: the engine records Future.Get's ActivityError, which carries the activity type and id, as the failure; a wrap would put a generic wrapError failure on top and add no context
}

// Simulations holds the simulation activity: the configuration worker's
// per-operation run.
type Simulations struct {
	Worker *configuration.SimulationWorker
}

// Simulate runs the operation's simulation as its requester, in one
// transaction with its evidence, audit entries and events.
func (s *Simulations) Simulate(ctx context.Context, in SimulateInput) error {
	_, err := s.Worker.Simulate(ctx, in.TenantID, in.OperationID)
	return err
}
