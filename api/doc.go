// Package api holds the platform's API contracts: openapi.yaml (OpenAPI 3.1),
// the source of truth for server interfaces and clients (ADR-0013), linted
// with vacuum-ruleset.yaml, asyncapi.yaml for domain events, and
// control.yaml for the operators' control plane on the admin listener. The
// package's tests enforce the rules of docs/design/04-api-design.md on the
// contracts and check every x-permission against the ref.permission seed in
// db/migrations.
package api

//go:generate go tool ogen --target gen --package gen --clean openapi.yaml
