package configuration

import (
	"context"
	"encoding/json"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// GetSchema implements GET /v1/schemas/{entity} (ADR-0046): the entity's
// custom field schema in the active configuration. A schema changes only
// when a configuration version is activated, so the configuration version
// is its entity tag. The authorizer has checked the entity's read
// permission (x-entity-permissions), and refused an entity it does not list.
func (e *Endpoints) GetSchema(ctx context.Context, params gen.GetSchemaParams) (gen.GetSchemaRes, error) {
	c, n, err := active(ctx)
	if err != nil {
		return nil, err
	}
	var s extensions.Schema
	ok := false
	if c != nil { // a tenant without a configuration has no custom fields
		s, ok = c.Extensions[params.Entity]
	}
	if !ok {
		return nil, problem.NotFound("The tenant has no custom field schema for " + params.Entity + ".")
	}
	tag := etag.Format(n)
	if !etag.NoneMatch(params.IfNoneMatch.Or(""), tag) {
		return &gen.GetSchemaNotModified{ETag: gen.NewOptString(tag)}, nil
	}
	doc, _ := json.Marshal(s) // a schema is strings, integers and booleans: it always encodes
	return &gen.CustomFieldSchemaHeaders{ETag: gen.NewOptString(tag), Response: gen.CustomFieldSchema{
		Entity: params.Entity, Version: int32(s.Version), ConfigVersion: n, Schema: object[gen.CustomFieldSchemaSchema](doc),
	}}, nil
}
