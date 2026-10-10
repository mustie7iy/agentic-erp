package apitest

import (
	"context"
	"encoding/json"
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
		_, file, _, _ := runtime.Caller(0)
		b, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../../../../api/asyncapi.yaml"))
		if err != nil {
			contractErr = err
			return
		}
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
		if contractErr = yaml.Unmarshal(b, &doc); contractErr != nil {
			return
		}
		contract = map[string]payloadSchema{}
		for _, m := range doc.Components.Messages {
			for _, part := range m.Headers.AllOf {
				if typ := part.Properties.Type.Const; typ != "" {
					contract[typ] = doc.Components.Schemas[filepath.Base(m.Payload.Ref)]
				}
			}
		}
	})
	return contract, contractErr
}

// Published returns the payloads of the tenant's events of one type in the
// order they were written, and fails the test if any payload lacks a field
// the event contract requires or has one it does not declare.
func Published(t testing.TB, ten pgtest.Tenant, e outbox.Event) []map[string]any {
	t.Helper()
	schemas, err := eventContract()
	if err != nil {
		t.Fatal(err)
	}
	typ := e.Type()
	schema, ok := schemas[typ]
	if !ok {
		t.Fatalf("event type %s is not in api/asyncapi.yaml", typ)
	}
	table := pgx.Identifier{e.Context, "outbox"}.Sanitize()
	rows, err := pgtest.Admin(t).Query(context.Background(), `SELECT payload FROM `+table+`
		WHERE tenant_id = $1 AND event_type = $2 ORDER BY id`, ten.ID, typ)
	if err != nil {
		t.Fatal(err)
	}
	payloads, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, raw := range payloads {
		var p map[string]any
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		for _, f := range schema.Required {
			if _, ok := p[f]; !ok {
				t.Errorf("%s: required field %q missing in %s", typ, f, raw)
			}
		}
		for f := range p {
			if _, ok := schema.Properties[f]; !ok {
				t.Errorf("%s: field %q is not in the contract (%v)", typ, f, slices.Sorted(maps.Keys(schema.Properties)))
			}
		}
		out = append(out, p)
	}
	return out
}
