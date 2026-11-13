package grc

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// What a control is linked to (ADR-0078 §7).
const (
	KindPermission        = "permission"
	KindSodRule           = "sod_rule"
	KindConfigurationPath = "configuration_path"
)

// Removed is the status of a link a person removed; an active link is
// Active, as a control is.
const Removed = "removed"

// Why a link does not hold (ADR-0078 §8).
const (
	ReasonNoPermission = "permission_not_in_catalog"
	ReasonRuleNotFound = "sod_rule_not_found"
	ReasonRuleNotBegun = "sod_rule_not_started"
	ReasonRuleEnded    = "sod_rule_ended"
	ReasonPathNoValue  = "configuration_path_without_value"
)

// maxPath is the longest configuration path a link names.
const maxPath = 500

// SodRule is a segregation-of-duties rule as the context shows it on a
// link: its pair, why it conflicts and its validity, ValidTo exclusive
// and nil while the rule has no end.
type SodRule struct {
	PermissionA, PermissionB, Description string
	ValidFrom                             time.Time
	ValidTo                               *time.Time
}

// Access reads the permission catalog and the tenant's
// segregation-of-duties rules (identity context). Permissions gives the
// description of each code the catalog holds; SodRules each rule the
// tenant has. What is not there is absent from the answer.
type Access interface {
	Permissions(ctx context.Context, codes []string) (map[string]string, error)
	SodRules(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]SodRule, error)
}

// Settings reads the tenant's active configuration version
// (configuration context): its number, and for each path, written as a
// configuration change names a setting, whether it has a value there.
type Settings interface {
	Present(ctx context.Context, paths []string) (version int32, present map[string]bool, err error)
}

// Link is a row of grc.control_link with its control's code and, once
// judged, whether what it names is there now. Target is the permission
// code, the rule's id or the configuration path, by Kind.
type Link struct {
	ID          uuid.UUID
	ControlID   uuid.UUID
	ControlCode string
	Kind        string
	Target      string
	Status      string
	Version     int32
	CreatedAt   time.Time
	CreatedBy   uuid.UUID
	UpdatedAt   time.Time
	UpdatedBy   uuid.UUID

	Holds         bool
	Reason        string  // why it does not hold; empty when it does
	Permission    *string // the permission's description, when the catalog has it
	Rule          *SodRule
	ConfigVersion int32 // the active configuration version a path was looked up in
}

// linkSelect reads links under the alias l with their control's code.
const linkSelect = `SELECT l.id, l.control_id, c.code, l.kind, l.target, l.status, l.version, l.created_at, l.created_by, l.updated_at, l.updated_by
	FROM grc.control_link l JOIN grc.control c ON c.tenant_id = l.tenant_id AND c.id = l.control_id`

func scanLink(row pgx.Row) (Link, error) {
	var l Link
	err := row.Scan(&l.ID, &l.ControlID, &l.ControlCode, &l.Kind, &l.Target, &l.Status, &l.Version, &l.CreatedAt, &l.CreatedBy, &l.UpdatedAt, &l.UpdatedBy)
	return l, errs.Wrapf(err, "scan control link")
}

// NewLink is a link as a client makes it: the field of its kind alone.
type NewLink struct {
	Kind              string
	PermissionCode    *string
	SodRuleID         *uuid.UUID
	ConfigurationPath *string
}

// errLink refuses a link whose fields do not fit its kind.
func errLink(field, message string) *problem.Problem {
	return problem.Invalid("grc.invalid_link", field, message)
}

// target is what the link names, by its kind: exactly the field of the
// kind is given.
func (in NewLink) target() (string, error) {
	if err := oneOf("kind", in.Kind, []string{KindPermission, KindSodRule, KindConfigurationPath}); err != nil {
		return "", err
	}
	given := map[string]bool{KindPermission: in.PermissionCode != nil, KindSodRule: in.SodRuleID != nil, KindConfigurationPath: in.ConfigurationPath != nil}
	fields := map[string]string{KindPermission: "permission_code", KindSodRule: "sod_rule_id", KindConfigurationPath: "configuration_path"}
	for _, kind := range []string{KindPermission, KindSodRule, KindConfigurationPath} {
		switch {
		case kind == in.Kind && !given[kind]:
			return "", errLink(fields[kind], "A link of the kind "+in.Kind+" names its "+fields[kind]+".")
		case kind != in.Kind && given[kind]:
			return "", errLink(fields[kind], "A link of the kind "+in.Kind+" names its "+fields[in.Kind]+" alone.")
		}
	}
	switch in.Kind {
	case KindPermission:
		return *in.PermissionCode, nil
	case KindSodRule:
		return in.SodRuleID.String(), nil
	}
	path := *in.ConfigurationPath
	if path == "" || path != strings.TrimSpace(path) || len(path) > maxPath {
		return "", errLink("configuration_path", "A configuration path is given without white space around it, in at most 500 characters.")
	}
	return path, nil
}

// judged says for each link whether what it names is there on the
// database's date today, which it returns: the permission in the
// catalog, the rule in force, a value at the path in the active
// configuration version (ADR-0078 §8). Each other context is asked once,
// and only when a link names something of it.
func (e *Endpoints) judged(ctx context.Context, tx pgx.Tx, links []Link) ([]Link, time.Time, error) {
	today, err := database.Today(ctx, tx)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("grc: date of the links' state: %w", err)
	}
	var codes, paths []string
	var rules []uuid.UUID
	for _, l := range links {
		switch l.Kind {
		case KindPermission:
			codes = append(codes, l.Target)
		case KindSodRule:
			rules = append(rules, uuid.MustParse(l.Target)) // the column's check holds a rule's id
		default:
			paths = append(paths, l.Target)
		}
	}
	there, err := e.there(ctx, codes, rules, paths)
	if err != nil {
		return nil, time.Time{}, err
	}
	out := make([]Link, len(links))
	for i, l := range links {
		out[i] = there.judge(l, today)
	}
	return out, today, nil
}

// there is what the other contexts hold of what links name.
type there struct {
	permissions map[string]string
	rules       map[uuid.UUID]SodRule
	version     int32
	present     map[string]bool
}

// there asks identity and the configuration what they hold of the
// codes, rules and paths, each once and only when asked about something.
func (e *Endpoints) there(ctx context.Context, codes []string, rules []uuid.UUID, paths []string) (there, error) {
	var t there
	var err error
	if len(codes) > 0 {
		slices.Sort(codes)
		if t.permissions, err = e.Access.Permissions(ctx, slices.Compact(codes)); err != nil {
			return there{}, fmt.Errorf("grc: permissions of %d links: %w", len(codes), err)
		}
	}
	if len(rules) > 0 {
		if t.rules, err = e.Access.SodRules(ctx, rules); err != nil {
			return there{}, fmt.Errorf("grc: segregation-of-duties rules of %d links: %w", len(rules), err)
		}
	}
	if len(paths) > 0 {
		slices.Sort(paths)
		if t.version, t.present, err = e.Settings.Present(ctx, slices.Compact(paths)); err != nil {
			return there{}, fmt.Errorf("grc: configuration paths of %d links: %w", len(paths), err)
		}
	}
	return t, nil
}

// judge is the link with whether it holds on the day, and why not.
func (t there) judge(l Link, today time.Time) Link {
	switch l.Kind {
	case KindPermission:
		if d, ok := t.permissions[l.Target]; ok {
			l.Permission, l.Holds = &d, true
		} else {
			l.Reason = ReasonNoPermission
		}
	case KindSodRule:
		r, ok := t.rules[uuid.MustParse(l.Target)] // the column's check holds a rule's id
		switch {
		case !ok:
			l.Reason = ReasonRuleNotFound
		case today.Before(r.ValidFrom):
			l.Rule, l.Reason = &r, ReasonRuleNotBegun
		case r.ValidTo != nil && !today.Before(*r.ValidTo):
			l.Rule, l.Reason = &r, ReasonRuleEnded
		default:
			l.Rule, l.Holds = &r, true
		}
	default:
		l.ConfigVersion = t.version
		if t.present[l.Target] {
			l.Holds = true
		} else {
			l.Reason = ReasonPathNoValue
		}
	}
	return l
}

// linkable refuses a link to what is not there when it is made: a
// permission outside the catalog, a rule the tenant does not have, a
// path without a value. A rule not in force yet, or ended, can be
// linked: the matrix shows the link as broken.
func linkable(l Link) error {
	switch l.Reason {
	case ReasonNoPermission:
		return problem.Invalid("grc.unknown_permission", "permission_code", "Permission "+l.Target+" is not in the permission catalog.")
	case ReasonRuleNotFound:
		return problem.ReferenceNotFound("sod_rule_id", "segregation-of-duties rule", "no segregation-of-duties rule with this id")
	case ReasonPathNoValue:
		return problem.Invalid("grc.configuration_path_without_value", "configuration_path",
			fmt.Sprintf("The active configuration version %d has no value at %s.", l.ConfigVersion, l.Target))
	}
	return nil
}

// Link links an active control to what enforces it (ADR-0078 §7, T98b):
// a permission, a segregation-of-duties rule or a configuration path
// that is there now. The control's row is locked, so a link is never
// made on a control retired meanwhile; its version does not move. A
// second active link to the target is refused by the database, 409
// grc.control_link_exists.
func (e *Endpoints) Link(ctx context.Context, controlID uuid.UUID, in NewLink) (Link, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Link{}, err
	}
	target, err := in.target()
	if err != nil {
		return Link{}, err
	}
	c, err := control(ctx, tx, controlID, true)
	if err != nil {
		return Link{}, err
	}
	if c.Status != Active {
		return Link{}, errRetired(c.Code)
	}
	l := Link{ID: uuid.Must(uuid.NewV7()), ControlID: c.ID, ControlCode: c.Code, Kind: in.Kind, Target: target, Status: Active, Version: 1,
		CreatedBy: p.ID, UpdatedBy: p.ID}
	judged, _, err := e.judged(ctx, tx, []Link{l})
	if err != nil {
		return Link{}, err
	}
	l = judged[0]
	if err := linkable(l); err != nil {
		return Link{}, err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO grc.control_link (tenant_id, id, control_id, kind, target, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $6) RETURNING created_at, updated_at`, p.TenantID, l.ID, c.ID, l.Kind, l.Target, p.ID).
		Scan(&l.CreatedAt, &l.UpdatedAt); err != nil {
		return Link{}, fmt.Errorf("grc: link control %s to %s %s: %w", c.Code, l.Kind, l.Target, err)
	}
	return l, changed(ctx, c, change{action: "link", name: "linked", link: &l,
		detail: map[string]any{"code": c.Code, "link_id": l.ID, "kind": l.Kind, "target": l.Target}})
}

// link reads a link of the control, locked for its removal; another
// control's link is not found.
func link(ctx context.Context, tx pgx.Tx, controlID, id uuid.UUID) (Link, error) {
	l, err := scanLink(tx.QueryRow(ctx, linkSelect+` WHERE l.id = $1 AND l.control_id = $2 FOR UPDATE OF l`, id, controlID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Link{}, problem.NotFoundOf("control link")
	case err != nil:
		return Link{}, fmt.Errorf("grc: read link %s of control %s: %w", id, controlID, err)
	}
	return l, nil
}

// Unlink removes a link of an active control, once, with the caller's
// version of the link (ADR-0078 §7): the row is kept as removed (04
// §4.3, no hard deletes) and the target can be linked again. The
// control is locked before the link, the order Link and Retire take.
func (e *Endpoints) Unlink(ctx context.Context, controlID, id uuid.UUID, ifMatch func(int32) error) (Link, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Link{}, err
	}
	c, err := control(ctx, tx, controlID, true)
	if err != nil {
		return Link{}, err
	}
	l, err := link(ctx, tx, c.ID, id)
	if err != nil {
		return Link{}, err
	}
	if err := ifMatch(l.Version); err != nil {
		return Link{}, err
	}
	switch {
	case c.Status != Active:
		return Link{}, errRetired(c.Code)
	case l.Status != Active:
		return Link{}, problem.Conflict("grc.control_link_removed", "The link was removed already.")
	}
	if err := tx.QueryRow(ctx, `UPDATE grc.control_link SET status = 'removed', version = version + 1, updated_by = $2
		WHERE id = $1 AND status = 'active' RETURNING status, version, updated_at, updated_by`, id, p.ID).
		Scan(&l.Status, &l.Version, &l.UpdatedAt, &l.UpdatedBy); err != nil {
		return Link{}, fmt.Errorf("grc: remove link %s of control %s: %w", id, c.Code, err)
	}
	judged, _, err := e.judged(ctx, tx, []Link{l})
	if err != nil {
		return Link{}, err
	}
	return judged[0], changed(ctx, c, change{action: "unlink", name: "unlinked", link: &l,
		detail: map[string]any{"code": c.Code, "link_id": l.ID, "kind": l.Kind, "target": l.Target}})
}
