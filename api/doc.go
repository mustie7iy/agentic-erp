// Package api holds the platform's API contracts: openapi.yaml (OpenAPI 3.1),
// the source of truth for server interfaces and clients (ADR-0013), linted
// with vacuum-ruleset.yaml, asyncapi.yaml for domain events, and
// control.yaml for the operators' control plane on the admin listener. The
// package's tests enforce the rules of docs/design/04-api-design.md on the
// contracts and check every x-permission against the ref.permission seed in
// db/migrations. Their lifecycle (deprecation with 365 days' notice,
// accepted breaking changes, events additive within a version) is ADR-0041:
// Operation.Lifecycle and EventChanges carry it.
package api

//go:generate go tool ogen --target gen --package gen --clean openapi.yaml
