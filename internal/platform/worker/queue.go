package worker

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Queue is a worker that drains per-tenant queues (ADR-0012's polling
// workers: archiving and deletion runs, evidence and cash exports, bank
// files, proposals, simulations): a round lists the tenants with work,
// then takes each tenant's next item until it has none. Each item is the
// worker's own (One claims it FOR UPDATE SKIP LOCKED in the tenant's
// transaction, so replicas share the queue); Queue is the loop around it
// they all repeated.
type Queue struct {
	// Name prefixes the round's errors, for example "records archiver".
	Name string
	// Tenants lists the tenants with work (database.Tenants).
	Tenants func(ctx context.Context) ([]uuid.UUID, error)
	// One takes the tenant's next item and reports whether there was one.
	One func(ctx context.Context, tenant uuid.UUID) (bool, error)
}

// RunOnce drains every tenant's queue and returns how many items it took.
// A tenant whose item fails is left for the next round and the others go
// on; the failures are returned together. A stopping worker (ctx ended)
// claims nothing more.
func (q Queue) RunOnce(ctx context.Context) (int, error) {
	tenants, err := q.Tenants(ctx)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", q.Name, err)
	}
	n := 0
	var errs []error
	for _, t := range tenants {
		for ctx.Err() == nil {
			did, err := q.One(ctx, t)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: tenant %s: %w", q.Name, t, err))
				break
			}
			if !did {
				break
			}
			n++
		}
	}
	return n, errors.Join(errs...)
}

// Round is RunOnce as a Loop round. Every queue was drained, so the next
// round waits for the interval.
func (q Queue) Round(ctx context.Context) (bool, error) {
	_, err := q.RunOnce(ctx)
	return false, err
}
