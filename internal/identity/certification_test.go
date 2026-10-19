package identity_test

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestAutonomyCertification: an agent above level 0 needs its type
// certified for its level on the model its alias routes to; an agent from
// before certification, with no type, keeps its level but is raised by
// none (ADR-0039).
func TestAutonomyCertification(t *testing.T) {
	c, ten := admin(t)
	pgtest.Route(t, ten.ID, "erp-default", pgtest.Certify(t, "ap_exceptions", 1))
	const uncertified = "identity.autonomy_uncertified"
	profile := func(level, agentType, alias string) string {
		return `{"autonomy_level": ` + level + agentType + `, "model": "` + alias + `", "description": "AP"}`
	}
	const ap = `, "agent_type": "ap_exceptions"`
	for name, body := range map[string]string{
		"no type":          profile("1", "", "erp-default"),
		"above the type's": profile("2", ap, "erp-default"),
		"on another model": profile("1", ap, "erp-analytics"),
		"a type, not its'": profile("1", `, "agent_type": "migration"`, "erp-default"),
	} {
		t.Run("create "+name, func(t *testing.T) {
			c.In(t).Problem(422, uncertified, "POST", "/v1/principals", `{"kind": "agent", "display_name": "A", "agent_profile": `+body+`}`, "")
		})
	}
	agent, _ := c.Must(201, "POST", "/v1/principals", `{"kind": "agent", "display_name": "A", "agent_profile": `+profile("1", ap, "erp-default")+`}`, "")
	path := "/v1/principals/" + id(agent)
	c.Problem(422, uncertified, "PATCH", path, `{"agent_profile": `+profile("2", ap, "erp-default")+`}`, `"1"`)
	c.Problem(422, uncertified, "PATCH", path, `{"agent_profile": `+profile("1", ap, "erp-analytics")+`}`, `"1"`)
	// A type cleared, or changed to one never certified, keeps nothing.
	c.Problem(422, uncertified, "PATCH", path, `{"agent_profile": `+profile("1", "", "erp-default")+`}`, `"1"`)
	c.Problem(422, uncertified, "PATCH", path, `{"agent_profile": `+profile("1", `, "agent_type": "migration"`, "erp-default")+`}`, `"1"`)
	c.Must(200, "PATCH", path, `{"agent_profile": {"autonomy_level": 1, "agent_type": "ap_exceptions", "model": "erp-default", "description": "Changed"}}`, `"1"`)
	c.Must(200, "PATCH", path, `{"agent_profile": `+profile("0", ap, "erp-analytics")+`}`, `"2"`)

	// The tenant's agent from before (level 2, no type) keeps its level,
	// on another alias too, and is raised by none.
	before := "/v1/principals/" + ten.Agent.ID.String()
	c.Must(200, "PATCH", before, `{"agent_profile": `+profile("2", "", "erp-analytics")+`}`, `"1"`)
	c.Problem(422, uncertified, "PATCH", before, `{"agent_profile": `+profile("3", "", "erp-analytics")+`}`, `"2"`)
}
