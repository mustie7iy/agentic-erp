package apitest

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"

	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// payloadSchema is what the event contract says about one payload.
type payloadSchema struct {
	Required   []string       `yaml:"required"`
	Properties map[string]any `yaml:"properties"`
}

var (
	contractOnce sync.Once
	contract     map[string]payloadSchema // by CloudEvents type
	contractErr  error
)

// eventContract reads api/asyncapi.yaml: each message's type constant and
// payload schema.
func eventContract() (map[string]payloadSchema, error) {
	contractOnce.Do(func() {
		b, err := contractFile()
		if err != nil {
			contractErr = err
			return
		}
		contract, _, contractErr = contractOf(b)
	})
	return contract, contractErr
}

// contractFile is api/asyncapi.yaml, found from this file's directory so
// that any package's test reads it.
func contractFile() ([]byte, error) {
	_, file, _, _ := runtime.Caller(0)
	b, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../../../api/asyncapi.yaml"))
	if err != nil {
		return nil, fmt.Errorf("apitest: read the event contract: %w", err)
	}
	return b, nil
}

// contractOf reads an event contract: each message type's payload schema,
// and the name of that schema.
func contractOf(b []byte) (map[string]payloadSchema, map[string]string, error) {
	var doc struct {
		Components struct {
			Messages map[string]struct {
				Headers struct {
					AllOf []struct {
						Properties struct {
							Type struct {
								Const string `yaml:"const"`
							} `yaml:"type"`
						} `yaml:"properties"`
					} `yaml:"allOf"`
				} `yaml:"headers"`
				Payload struct {
					Ref string `yaml:"$ref"`
				} `yaml:"payload"`
			} `yaml:"messages"`
			Schemas map[string]payloadSchema `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, nil, fmt.Errorf("apitest: parse event contract: %w", err)
	}
	schemas, names := map[string]payloadSchema{}, map[string]string{}
	for _, m := range doc.Components.Messages {
		for _, part := range m.Headers.AllOf {
			if typ := part.Properties.Type.Const; typ != "" {
				names[typ] = filepath.Base(m.Payload.Ref)
				schemas[typ] = doc.Components.Schemas[names[typ]]
			}
		}
	}
	return schemas, names, nil
}

// Published returns the payloads of the tenant's events of one type in the
// order they were written, and fails the test if any payload lacks a field
// the event contract requires or has one it does not declare.
func Published(tb testing.TB, ten pgtest.Tenant, e outbox.Event) []map[string]any {
	tb.Helper()
	schemas, err := eventContract()
	if err != nil {
		tb.Fatal(err)
	}
	typ := e.Type()
	schema, ok := schemas[typ]
	if !ok {
		tb.Fatalf("event type %s is not in api/asyncapi.yaml", typ)
	}
	table := pgx.Identifier{e.Context, "outbox"}.Sanitize()
	rows, err := pgtest.Admin(tb).Query(context.Background(), `SELECT payload FROM `+table+`
		WHERE tenant_id = $1 AND event_type = $2 ORDER BY id`, ten.ID, typ)
	if err != nil {
		tb.Fatal(err)
	}
	payloads, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		tb.Fatal(err)
	}
	var out []map[string]any
	for _, raw := range payloads {
		var p map[string]any
		if err := json.Unmarshal(raw, &p); err != nil {
			tb.Fatal(err)
		}
		for _, f := range schema.Required {
			if _, ok := p[f]; !ok {
				tb.Errorf("%s: required field %q missing in %s", typ, f, raw)
			}
		}
		for f := range p {
			if _, ok := schema.Properties[f]; !ok {
				tb.Errorf("%s: field %q is not in the contract (%v)", typ, f, slices.Sorted(maps.Keys(schema.Properties)))
			}
		}
		out = append(out, p)
	}
	return out
}

// NeverPublished fails tb when any event the tenant's context wrote to its
// outbox carries value anywhere in its payload: a personal or sensitive
// custom field is masked in every event (ADR-0046).
func NeverPublished(tb testing.TB, ten pgtest.Tenant, boundedContext, value string) {
	tb.Helper()
	var n int
	if err := pgtest.Admin(tb).QueryRow(context.Background(), `SELECT count(*) FROM `+pgx.Identifier{boundedContext, "outbox"}.Sanitize()+`
		WHERE tenant_id = $1 AND strpos(payload::text, $2) > 0`, ten.ID, value).Scan(&n); err != nil {
		tb.Fatal(err)
	}
	if n != 0 {
		tb.Errorf("%s outbox: got %d events carrying a masked value, want none", boundedContext, n)
	}
}
