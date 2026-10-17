package database

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// RequireVerifiedTLS reports whether url, as pgx parses it (with the PG*
// environment variables it reads), reaches every host it may try over TLS
// that verifies the server's certificate and name: sslmode=verify-full.
// Production refuses anything else (ADR-0033): prefer and allow may fall
// back to plaintext, require and verify-ca accept a certificate for any
// name.
func RequireVerifiedTLS(url string) error {
	cfg, err := pgconn.ParseConfig(url)
	if err != nil {
		// pgconn's parse error redacts the password.
		return fmt.Errorf("database: %w", err)
	}
	attempts := append([]*pgconn.FallbackConfig{{TLSConfig: cfg.TLSConfig}}, cfg.Fallbacks...)
	for _, a := range attempts {
		if a.TLSConfig == nil {
			return errors.New("sslmode must be verify-full: a connection may be made without TLS")
		}
	}
	for _, a := range attempts {
		if a.TLSConfig.InsecureSkipVerify {
			return errors.New("sslmode must be verify-full: the server's certificate or name is not verified")
		}
	}
	return nil
}
