package inventory

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// An outbound delivery's stock (ADR-0083 §5): shipping checks a pick
// against what a storage location holds (Held) and posts the delivery's
// goods issue through IssueForDelivery (S2b1).

// Held is what a storage location of a plant holds of a material now, in
// its base unit as a plain decimal, "0" when it has never held any. It
// reads the stock as it is, locking nothing: a pick reserves nothing, and
// the goods issue takes the stock under its locks.
func (e *Endpoints) Held(ctx context.Context, material, plant, storageLocation uuid.UUID) (string, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return "", err
	}
	var q string
	err = tx.QueryRow(ctx, `SELECT trim_scale(quantity)::text FROM inventory.stock WHERE material_id = $1 AND plant_id = $2 AND storage_location_id = $3`,
		material, plant, storageLocation).Scan(&q)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "0", nil
	case err != nil:
		return "", fmt.Errorf("inventory: stock of material %s at storage location %s: %w", material, storageLocation, err)
	}
	return q, nil
}
