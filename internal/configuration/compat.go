package configuration

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Upgrade compatibility (06 §6.4): an activated document may carry keys a
// newer replica knows and this one does not. Reading the active document
// for use (settings, the regression capture) ignores them, each logged once
// per process, never as a nil document. Assessing or storing a change does
// not: a base or a proposal with such a key is refused here (503
// config.unknown_keys, retryable), as this replica would assess the change
// without the key, or store it without it.

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

func readVersion(ctx context.Context, tx pgx.Tx, number int32) ([]byte, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT document FROM config.config_version WHERE number = $1`, number).Scan(&raw); err != nil {
		return nil, fmt.Errorf("configuration: read version %d: %w", number, err)
	}
	return raw, nil
}

// versionDocument reads the document of an activated version for use.
func versionDocument(ctx context.Context, tx pgx.Tx, number int32) (*config.Config, error) {
	raw, err := readVersion(ctx, tx, number)
	if err != nil {
		return nil, err
	}
	c, err := activeConfig(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("configuration: read version %d: %w", number, err)
	}
	return c, nil
}

// assessableVersion reads the version a change is based on, which this
// replica must read whole to assess the change.
func assessableVersion(ctx context.Context, tx pgx.Tx, number int32) (*config.Config, error) {
	raw, err := readVersion(ctx, tx, number)
	if err != nil {
		return nil, err
	}
	return whole(raw, fmt.Sprintf("Version %d", number))
}

// storedProposal reads a submitted proposal, which a newer replica may
// have accepted with keys this one does not know.
func storedProposal(raw []byte) (*config.Config, error) {
	return whole(raw, "The proposal")
}

// whole reads a document this replica must read without ignoring a key.
func whole(raw []byte, what string) (*config.Config, error) {
	c, unknown, err := config.ParseActive(raw)
	if err != nil || len(unknown) > 0 {
		p := problem.New(http.StatusServiceUnavailable, "config.unknown_keys", "Service unavailable",
			what+" has configuration this replica cannot read; retry: an upgraded replica will take it.")
		p.RetryAfter = 5
		return nil, p
	}
	return c, nil
}
