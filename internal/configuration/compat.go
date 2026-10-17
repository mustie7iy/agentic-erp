package configuration

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
)

// Upgrade compatibility (06 §6.4): an activated document may carry keys a
// newer replica knows and this one does not. It is read without them,
// each logged once per process, never as a nil document; a document that
// does not read even so fails the call. Submitted documents are read
// strictly (parse).

var unknownLogged sync.Map

// activeConfig reads an activated document tolerantly.
func activeConfig(ctx context.Context, raw []byte) (*config.Config, error) {
	c, unknown, err := config.ParseActive(raw)
	if err != nil {
		return nil, err
	}
	for _, u := range unknown {
		if _, seen := unknownLogged.LoadOrStore(u, struct{}{}); !seen {
			slog.WarnContext(ctx, "the active configuration has a key this binary does not know; it is ignored until this replica is upgraded", "finding", u)
		}
	}
	return c, nil
}

// versionDocument reads the document of an activated version.
func versionDocument(ctx context.Context, tx pgx.Tx, number int32) (*config.Config, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT document FROM config.config_version WHERE number = $1`, number).Scan(&raw); err != nil {
		return nil, fmt.Errorf("configuration: read version %d: %w", number, err)
	}
	c, err := activeConfig(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("configuration: read version %d: %w", number, err)
	}
	return c, nil
}
