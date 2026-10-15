// Package workflow is the platform's port to the workflow engine (ADR-0012):
// long-running work runs as Temporal workflows whose activities are the
// application's services in a unit of work. The API sees a workflow as an
// Operation; this package starts, cancels and hosts them.
package workflow

import (
	"context"
	"errors"
	"fmt"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

// Start describes a workflow to start: its id (derived from the business
// key, so a second start of the same work is refused), the task queue, the
// registered workflow name and its arguments.
type Start struct {
	ID    string
	Queue string
	Name  string
	Args  []any
}

// Client starts and cancels workflows.
type Client interface {
	// Start starts the workflow; started is false when one with the id
	// already ran or runs, which is not an error (idempotent dispatch).
	Start(ctx context.Context, s Start) (started bool, err error)
	// Cancel requests the workflow's cancellation; a workflow that does not
	// exist is not an error (the work may have finished).
	Cancel(ctx context.Context, id string) error
}

// Config locates the engine.
type Config struct {
	Address   string // host:port of the Temporal frontend
	Namespace string // one per environment (ADR-0012)
	Codec     *Codec // seals payloads before they reach the engine; nil: plain
}

// Temporal is the Client on a Temporal cluster, and hosts workers.
type Temporal struct {
	c client.Client
}

// Dial connects; the SDK checks the cluster answers before returning.
func Dial(ctx context.Context, cfg Config) (*Temporal, error) {
	c, err := client.DialContext(ctx, client.Options{HostPort: cfg.Address, Namespace: cfg.Namespace, DataConverter: dataConverter(cfg.Codec)})
	if err != nil {
		return nil, fmt.Errorf("workflow: connecting to %s: %w", cfg.Address, err)
	}
	return &Temporal{c: c}, nil
}

// Start implements Client. The engine is asked whether the id is known
// first: a start against a known id can come back as the existing run
// rather than as an error, and an existing run, finished or not, means the
// work was dispatched already.
func (t *Temporal) Start(ctx context.Context, s Start) (bool, error) {
	_, err := t.c.DescribeWorkflowExecution(ctx, s.ID, "")
	var missing *serviceerror.NotFound
	switch {
	case err == nil:
		return false, nil
	case !errors.As(err, &missing):
		return false, fmt.Errorf("workflow: looking up %s: %w", s.ID, err)
	}
	// Two dispatchers racing past the lookup: the engine refuses the second
	// start, which is reported and found settled on the next round.
	if _, err = t.c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: s.ID, TaskQueue: s.Queue,
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE, WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_FAIL}, s.Name, s.Args...); err != nil {
		return false, fmt.Errorf("workflow: starting %s: %w", s.ID, err)
	}
	return true, nil
}

// Cancel implements Client.
func (t *Temporal) Cancel(ctx context.Context, id string) error {
	err := t.c.CancelWorkflow(ctx, id, "")
	var missing *serviceerror.NotFound
	if errors.As(err, &missing) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("workflow: cancelling %s: %w", id, err)
	}
	return nil
}

// Close releases the connection.
func (t *Temporal) Close() { t.c.Close() }

// Registry is what workflows and activities register themselves on.
type Registry = worker.Registry

// Worker hosts the workflows and activities registered by register on the
// task queue until ctx ends.
func (t *Temporal) Worker(ctx context.Context, queue string, register func(Registry)) error {
	w := worker.New(t.c, queue, worker.Options{})
	register(w)
	interrupt := make(chan interface{})
	go func() {
		<-ctx.Done()
		close(interrupt)
	}()
	if err := w.Run(interrupt); err != nil {
		return fmt.Errorf("workflow: worker on queue %s: %w", queue, err)
	}
	return nil
}
