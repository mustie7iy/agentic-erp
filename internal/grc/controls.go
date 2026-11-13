package grc

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// NewControl is a control as a client records it.
type NewControl struct {
	Code      string
	Objective string
	Process   string
	OwnerID   uuid.UUID
	Frequency string
	Nature    string
	// FrameworkReferences are the tenant's references to a framework it
	// names, each once; nil for none.
	FrameworkReferences []string
}

// references checks a control's framework references: texts without
// white space around them, each given once (ADR-0078 §9).
func references(refs []string) error {
	seen := map[string]bool{}
	for i, r := range refs {
		field := fmt.Sprintf("framework_references[%d]", i)
		switch {
		case r == "" || r != strings.TrimSpace(r):
			return problem.Invalid("grc.invalid_framework_reference", field, "A framework reference is a text without white space around it.")
		case seen[r]:
			return problem.Invalid("grc.invalid_framework_reference", field, "A framework reference is given once.")
		}
		seen[r] = true
	}
	return nil
}

// texts checks a control's objective and process: texts, the process as
// it is stored and filtered by, without surrounding white space.
func texts(objective, process string) error {
	if err := problem.RequireText("objective", objective); err != nil {
		return err
	}
	if err := problem.RequireText("process", process); err != nil {
		return err
	}
	if process != strings.TrimSpace(process) {
		return problem.Invalid("grc.invalid_process", "process", "A process is given without leading or trailing white space.")
	}
	return nil
}

// says checks what a control says before it is written: its texts, its
// frequency, its nature and its framework references. The owner is checked apart (owner), only
// when it is new.
func says(in NewControl) error {
	if err := texts(in.Objective, in.Process); err != nil {
		return err
	}
	if err := oneOf("frequency", in.Frequency, frequencies); err != nil {
		return err
	}
	if err := oneOf("nature", in.Nature, natures); err != nil {
		return err
	}
	return references(in.FrameworkReferences)
}

// Record records a control in the tenant's library (ADR-0078 §2,
// T98a), active, with its audit record and ControlChanged. A code
// already taken, whatever its case, is refused by the database, 409
// grc.control_code_taken.
func (e *Endpoints) Record(ctx context.Context, in NewControl) (Control, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Control{}, err
	}
	if err := says(in); err != nil {
		return Control{}, err
	}
	if err := e.owner(ctx, in.OwnerID); err != nil {
		return Control{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO grc.control (tenant_id, id, code, objective, process, owner_id, frequency, nature,
			framework_references, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)`,
		p.TenantID, id, in.Code, in.Objective, in.Process, in.OwnerID, in.Frequency, in.Nature, append([]string{}, in.FrameworkReferences...), p.ID); err != nil {
		return Control{}, fmt.Errorf("grc: record control %s: %w", in.Code, err)
	}
	c, err := control(ctx, tx, id, false)
	if err != nil {
		return Control{}, err
	}
	return c, changed(ctx, c, change{action: "create", name: "created", detail: map[string]any{"code": c.Code, "process": c.Process,
		"owner_id": c.OwnerID, "frequency": c.Frequency, "nature": c.Nature}})
}

// ControlChange is a merge patch of a control: nil fields stay.
type ControlChange struct {
	Objective *string
	Process   *string
	OwnerID   *uuid.UUID
	Frequency *string
	Nature    *string
	// FrameworkReferences replaces the control's references as a whole;
	// an empty list clears them.
	FrameworkReferences *[]string
}

// apply is the control after the change and the names of the fields
// that differ, in the contract's order.
func (ch ControlChange) apply(c Control) (NewControl, []string) {
	to := NewControl{Code: c.Code, Objective: c.Objective, Process: c.Process, OwnerID: c.OwnerID, Frequency: c.Frequency, Nature: c.Nature,
		FrameworkReferences: c.FrameworkReferences}
	var fields []string
	set := func(name string, differs bool) {
		if differs {
			fields = append(fields, name)
		}
	}
	if ch.Objective != nil {
		set("objective", *ch.Objective != c.Objective)
		to.Objective = *ch.Objective
	}
	if ch.Process != nil {
		set("process", *ch.Process != c.Process)
		to.Process = *ch.Process
	}
	if ch.OwnerID != nil {
		set("owner_id", *ch.OwnerID != c.OwnerID)
		to.OwnerID = *ch.OwnerID
	}
	if ch.Frequency != nil {
		set("frequency", *ch.Frequency != c.Frequency)
		to.Frequency = *ch.Frequency
	}
	if ch.Nature != nil {
		set("nature", *ch.Nature != c.Nature)
		to.Nature = *ch.Nature
	}
	if ch.FrameworkReferences != nil {
		set("framework_references", !slices.Equal(*ch.FrameworkReferences, c.FrameworkReferences))
		to.FrameworkReferences = append([]string{}, *ch.FrameworkReferences...)
	}
	return to, fields
}

// Change changes what an active control says, with the caller's
// version of it (ADR-0008, ADR-0078 §2): the row is locked, the version
// compared and the change recorded with the fields it changed. The code
// stays; the owner is checked only when the change names another.
func (e *Endpoints) Change(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, ch ControlChange) (Control, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Control{}, err
	}
	c, err := control(ctx, tx, id, true)
	if err != nil {
		return Control{}, err
	}
	if err := ifMatch(c.Version); err != nil {
		return Control{}, err
	}
	if c.Status != Active {
		return Control{}, errRetired(c.Code)
	}
	to, fields := ch.apply(c)
	if len(fields) == 0 {
		return Control{}, problem.Unprocessable("grc.nothing_to_change", "The change would leave the control as it is.")
	}
	if err := says(to); err != nil {
		return Control{}, err
	}
	if to.OwnerID != c.OwnerID {
		if err := e.owner(ctx, to.OwnerID); err != nil {
			return Control{}, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE grc.control SET objective = $2, process = $3, owner_id = $4, frequency = $5, nature = $6,
			framework_references = $7, version = version + 1, updated_by = $8 WHERE id = $1 AND status = 'active'`,
		id, to.Objective, to.Process, to.OwnerID, to.Frequency, to.Nature, to.FrameworkReferences, p.ID); err != nil {
		return Control{}, fmt.Errorf("grc: change control %s: %w", id, err)
	}
	after, err := control(ctx, tx, id, false)
	if err != nil {
		return Control{}, err
	}
	return after, changed(ctx, after, change{action: "change", name: "changed", detail: map[string]any{"code": after.Code, "fields": fields}})
}

// Retire retires an active control for good, once, with the
// caller's version of it: it changes no more and takes no further test.
func Retire(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Control, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Control{}, err
	}
	c, err := control(ctx, tx, id, true)
	if err != nil {
		return Control{}, err
	}
	if err := ifMatch(c.Version); err != nil {
		return Control{}, err
	}
	if c.Status != Active {
		return Control{}, errRetired(c.Code)
	}
	if _, err := tx.Exec(ctx, `UPDATE grc.control SET status = 'retired', version = version + 1, updated_by = $2
		WHERE id = $1 AND status = 'active'`, id, p.ID); err != nil {
		return Control{}, fmt.Errorf("grc: retire control %s: %w", id, err)
	}
	retired, err := control(ctx, tx, id, false)
	if err != nil {
		return Control{}, err
	}
	return retired, changed(ctx, retired, change{action: "retire", name: "retired", detail: map[string]any{"code": retired.Code}})
}

// change is one change of a control as it is recorded: the last word of
// its audit action, what the event calls it, the audit detail, and the
// test or the link it is about, nil for every other change.
type change struct {
	action, name string
	detail       map[string]any
	test         *Test
	link         *Link
}

// changed adds the audit record of a control's change and publishes
// ControlChanged in the same transaction (ADR-0007, ADR-0011; ADR-0078
// §5). Neither carries a test's note: free text of a person's, which
// may name people.
func changed(ctx context.Context, c Control, ch change) error {
	var test, link any
	if t := ch.test; t != nil {
		test = map[string]any{"test_id": t.ID, "tested_on": t.TestedOn.Format("2006-01-02"), "result": t.Result, "tested_by": t.TestedBy}
	}
	if l := ch.link; l != nil {
		link = map[string]any{"link_id": l.ID, "kind": l.Kind, "target": l.Target}
	}
	payload := map[string]any{"control_id": c.ID, "code": c.Code, "process": c.Process, "owner_id": c.OwnerID, "frequency": c.Frequency,
		"nature": c.Nature, "status": c.Status, "change": ch.name, "test": test, "link": link, "version": c.Version}
	if err := audit.Record(ctx, "grc.control."+ch.action, "grc.control", c.ID, ch.detail,
		outbox.Event{Context: "grc", Name: "control_changed", AggregateType: "grc.control", AggregateID: c.ID, Payload: payload}); err != nil {
		return fmt.Errorf("grc: record %s of control %s: %w", ch.action, c.ID, err)
	}
	return nil
}
