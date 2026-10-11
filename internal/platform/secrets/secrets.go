// Package secrets resolves secret references (02-architecture: KMS/Vault).
// A reference names where a secret lives instead of carrying it:
//
//	env:NAME                      an environment variable
//	file:/run/secrets/name        a file (mounted secrets), trailing newline trimmed
//	vault:secret/erp/field-key#value   a field of a HashiCorp Vault KV v2 secret
//
// Server settings that carry secrets accept a reference in place of the
// value, and connectors name their credentials by reference
// (`secret_ref`), so no credential has to sit in configuration, in the
// database or in the environment of every replica.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrNotFound means the reference names nothing.
var ErrNotFound = errors.New("secrets: not found")

// Resolver turns a reference into a secret.
type Resolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// Schemes of references.
const (
	SchemeEnv   = "env"
	SchemeFile  = "file"
	SchemeVault = "vault"
)

// IsRef reports whether a value is a reference of a known scheme.
func IsRef(value string) bool {
	scheme, _, ok := strings.Cut(value, ":")
	return ok && (scheme == SchemeEnv || scheme == SchemeFile || scheme == SchemeVault)
}

// Value resolves value when it is a reference and returns it as is
// otherwise, so a setting may carry either.
func Value(ctx context.Context, r Resolver, value string) (string, error) {
	if !IsRef(value) {
		return value, nil
	}
	return r.Resolve(ctx, value)
}

// Local resolves env: and file: references on the replica itself.
type Local struct {
	Getenv   func(string) string          // nil: os.Getenv
	ReadFile func(string) ([]byte, error) // nil: os.ReadFile
}

func (l Local) getenv(name string) string {
	if l.Getenv != nil {
		return l.Getenv(name)
	}
	return os.Getenv(name)
}

func (l Local) readFile(path string) ([]byte, error) {
	if l.ReadFile != nil {
		return l.ReadFile(path)
	}
	return os.ReadFile(path)
}

// Resolve implements Resolver.
func (l Local) Resolve(_ context.Context, ref string) (string, error) {
	scheme, rest, _ := strings.Cut(ref, ":")
	switch scheme {
	case SchemeEnv:
		if rest == "" {
			return "", fmt.Errorf("secrets: %s: %w (no variable named)", ref, ErrNotFound)
		}
		v := l.getenv(rest)
		if v == "" {
			return "", fmt.Errorf("secrets: %s: %w", ref, ErrNotFound)
		}
		return v, nil
	case SchemeFile:
		b, err := l.readFile(rest)
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("secrets: %s: %w", ref, ErrNotFound)
		}
		if err != nil {
			return "", fmt.Errorf("secrets: %s: %w", ref, err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return "", fmt.Errorf("secrets: %s: unsupported reference", ref)
}

// Multi routes each reference to the resolver of its scheme.
type Multi map[string]Resolver

// Resolve implements Resolver.
func (m Multi) Resolve(ctx context.Context, ref string) (string, error) {
	scheme, _, _ := strings.Cut(ref, ":")
	r, ok := m[scheme]
	if !ok {
		return "", fmt.Errorf("secrets: %s: no resolver for %q references", ref, scheme)
	}
	return r.Resolve(ctx, ref)
}
