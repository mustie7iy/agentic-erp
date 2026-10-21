// Package configuration is the configuration context (03-data-model §3.5):
// configuration as code. A tenant's settings are one document (the
// prototype's config.Config), activated as immutable, numbered versions.
// Changes are proposed as change requests: submitted, validated and
// classified by tier, supported by evidence where the tier requires it,
// approved, and activated by a person. Number ranges are maintained through
// the document and applied to config.number_range on activation.
//
// Tier 1 changes need no approval and are approved on submission; tier 2
// changes open approval requests in the workflow context; tier 3-4 changes
// first wait for simulation evidence, which the SimulationWorker records
// by replaying recent transactions under the proposal (:simulate).
package configuration

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/rules"
)

// Change request statuses.
const (
	Draft            = "draft"
	Validating       = "validating"
	Invalid          = "invalid"
	AwaitingEvidence = "awaiting_evidence"
	PendingApproval  = "pending_approval"
	Approved         = "approved"
	Active           = "active"
	Rejected         = "rejected"
	Withdrawn        = "withdrawn"
)

func init() {
	problem.RegisterConstraint("change_request_tenant_id_base_version_fkey", func() *problem.Problem {
		return problem.Unprocessable("config.unknown_base_version", "There is no configuration version with this number.",
			problem.FieldError{Field: "base_version", Code: "not_found", Message: "an activated configuration version"})
	})
}

// LegalEntities is what configuration needs to know about the tenancy
// context's legal entities, answered in the request's transaction.
type LegalEntities interface {
	LegalEntityByCode(ctx context.Context, code string) (id uuid.UUID, status string, ok bool, err error)
}

// Section is a part of the configuration document that another context
// owns and keeps in its own tables: the ledger's accounts, calendars and
// periods, the partner context's payment terms.
type Section interface {
	// Check reports what is wrong with the proposed document given the
	// tenant's data and the base document (nil for the first version).
	Check(ctx context.Context, base, proposed *config.Config) ([]config.Finding, error)
	// Apply makes the context's data match an activated document.
	Apply(ctx context.Context, doc *config.Config) error
}

// Opener opens and withdraws approval requests (workflow context).
type Opener interface {
	Open(ctx context.Context, in approvals.NewRequest) (uuid.UUID, bool, error)
	Withdraw(ctx context.Context, objectType string, objectID uuid.UUID, comment string) error
}

// Dependencies is what configuration needs from other contexts.
type Dependencies struct {
	LegalEntities LegalEntities
	Sections      []Section
	Approvals     Opener
	Simulator     Simulator // replays transactions for simulation evidence (the worker)
	Regressor     Regressor // captures and replays regression suites
	Actors        Actors    // who a queued simulation acts as (the worker)
	// RuleSamplers read each entity's recent rows, on which a simulation
	// replays proposed validation rules (ADR-0046), by entity.
	RuleSamplers map[string]rules.Sampler
	Disclosures  []Disclosure // what a reader is shown of evidence (the payables context's invoices by company, the analytics context's metric values)
}

// Actors reads who deferred work acts as: the person who queued it
// (identity context).
type Actors interface {
	Actor(ctx context.Context, tx pgx.Tx, id uuid.UUID) (reqctx.Principal, error)
}

// check validates a document for activation on top of base (nil for the
// first version): the document's own rules, its number ranges and every
// section, all findings reported together as one 422. Amounts are checked
// with the exponents of ref.currency (ADR-0005).
func (d Dependencies) check(ctx context.Context, tx pgx.Tx, base, proposed *config.Config) error {
	exponents, err := currency.Load(ctx, tx)
	if err != nil {
		return fmt.Errorf("configuration: check: %w", err)
	}
	findings := config.Validate(proposed, exponents)
	if base != nil {
		findings = append(findings, rangeTransitions(base, proposed)...)
	}
	findings = append(findings, config.ExtensionTransitions(base, proposed)...)
	companies, err := rangeCompanies(ctx, d.LegalEntities, proposed)
	if err != nil {
		return err
	}
	findings = append(findings, companies...)
	for _, s := range d.Sections {
		more, err := s.Check(ctx, base, proposed)
		if err != nil {
			return err
		}
		findings = append(findings, more...)
	}
	return failed(findings)
}

// Item is one computed difference of a change request.
type Item struct {
	No   int32   `json:"item_no"`
	Path string  `json:"path"`
	Old  *string `json:"old_value"`
	New  *string `json:"new_value"`
	Tier int16   `json:"tier"`
}

// Evidence is a recorded check result.
type Evidence struct {
	Kind       string          `json:"kind"`
	Passed     bool            `json:"passed"`
	Summary    string          `json:"summary"`
	Detail     json.RawMessage `json:"detail"`
	RecordedAt time.Time       `json:"recorded_at"`
	RecordedBy uuid.UUID       `json:"recorded_by"`
}

// ChangeRequest is a row of config.change_request with its items and evidence.
type ChangeRequest struct {
	ID                uuid.UUID
	Number            string
	Title             string
	Rationale         string
	AuthorID          uuid.UUID
	BaseVersion       int32
	Proposed          json.RawMessage
	Tier              int16
	Status            string
	WorkflowRequestID *uuid.UUID
	ActivatedVersion  *int32
	Items             []Item
	Evidence          []Evidence
	// Notes are for the reviewers of a generated proposal: a country pack
	// application's (ADR-0048), as it created the request.
	Notes     []config.Note
	Version   int32
	CreatedAt time.Time
	CreatedBy uuid.UUID
	UpdatedAt time.Time
	UpdatedBy uuid.UUID
}

const crSelect = `SELECT c.id, c.number, c.title, c.rationale, c.author_id, c.base_version, c.proposed, c.tier, c.status,
		c.workflow_request_id, c.activated_version, c.notes, c.version, c.created_at, c.created_by, c.updated_at, c.updated_by,
		coalesce((SELECT json_agg(json_build_object('item_no', i.item_no, 'path', i.path, 'old_value', i.old_value, 'new_value', i.new_value,
			'tier', i.tier) ORDER BY i.item_no) FROM config.change_item i WHERE i.tenant_id = c.tenant_id AND i.change_request_id = c.id), '[]'),
		coalesce((SELECT json_agg(json_build_object('kind', e.kind, 'passed', e.passed, 'summary', e.summary, 'detail', e.detail,
			'recorded_at', e.recorded_at, 'recorded_by', e.recorded_by) ORDER BY e.recorded_at, e.kind)
			FROM config.evidence e WHERE e.tenant_id = c.tenant_id AND e.change_request_id = c.id), '[]')
	FROM config.change_request c`

func scanCR(row pgx.Row) (ChangeRequest, error) {
	var c ChangeRequest
	err := row.Scan(&c.ID, &c.Number, &c.Title, &c.Rationale, &c.AuthorID, &c.BaseVersion, &c.Proposed, &c.Tier, &c.Status,
		&c.WorkflowRequestID, &c.ActivatedVersion, &c.Notes, &c.Version, &c.CreatedAt, &c.CreatedBy, &c.UpdatedAt, &c.UpdatedBy, &c.Items, &c.Evidence)
	return c, errs.Wrapf(err, "scan change request")
}

func getCR(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (ChangeRequest, error) {
	q := crSelect + ` WHERE c.id = $1`
	if lock {
		q += ` FOR UPDATE OF c`
	}
	c, err := scanCR(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, problem.NotFoundOf("change request")
	}
	if err != nil {
		return c, fmt.Errorf("configuration: read change request %s: %w", id, err)
	}
	return c, nil
}

func requireStatus(c ChangeRequest, action string, allowed ...string) error {
	for _, s := range allowed {
		if c.Status == s {
			return nil
		}
	}
	return problem.Conflict("config.invalid_status", fmt.Sprintf("The change request is %s; %s requires status %v.", c.Status, action, allowed))
}

func requireAuthor(c ChangeRequest, actor reqctx.Principal) error {
	if c.AuthorID != actor.ID {
		return problem.Forbidden("config.not_author", "Only the author of a change request may change or withdraw it.")
	}
	return nil
}

// activeVersion locks and returns the tenant's active version number;
// requests that create versions or change request numbers serialize on it.
func activeVersion(ctx context.Context, tx pgx.Tx) (int32, error) {
	var n int32
	err := tx.QueryRow(ctx, `SELECT number FROM config.active_version FOR UPDATE`).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, problem.Conflict("config.not_bootstrapped", "The tenant has no configuration yet.")
	}
	if err != nil {
		return n, fmt.Errorf("configuration: lock the active version: %w", err)
	}
	return n, nil
}

// NewChangeRequest is the input of CreateChangeRequest.
type NewChangeRequest struct {
	Title, Rationale string
	BaseVersion      int32
	Proposed         json.RawMessage
}

// CreateChangeRequest stores a draft. Its number is CR-NNNNNN, sequential
// per tenant. Items and tier are computed on submission.
func CreateChangeRequest(ctx context.Context, in NewChangeRequest) (ChangeRequest, error) {
	return create(ctx, in, nil, nil)
}

// create stores a draft with the notes of the proposal that generated it
// and audits it with the detail added.
func create(ctx context.Context, in NewChangeRequest, notes []config.Note, detail map[string]any) (ChangeRequest, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return ChangeRequest{}, err
	}
	if _, err := activeVersion(ctx, tx); err != nil {
		return ChangeRequest{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if notes == nil {
		notes = []config.Note{} // the column holds an array, never JSON null
	}
	noted, _ := json.Marshal(notes) // notes are strings
	if _, err := tx.Exec(ctx, `INSERT INTO config.change_request (tenant_id, id, number, title, rationale, author_id, base_version, proposed, tier, status,
			notes, created_by, updated_by)
		SELECT $1, $2, 'CR-' || lpad((count(*) + 1)::text, 6, '0'), $3, $4, $5, $6, $7, 1, 'draft', $8, $5, $5 FROM config.change_request`,
		actor.TenantID, id, in.Title, in.Rationale, actor.ID, in.BaseVersion, in.Proposed, json.RawMessage(noted)); err != nil {
		return ChangeRequest{}, fmt.Errorf("configuration: insert change request %s: %w", id, err)
	}
	c, err := getCR(ctx, tx, id, false)
	if err != nil {
		return ChangeRequest{}, err
	}
	audited := map[string]any{"number": c.Number, "base_version": c.BaseVersion}
	maps.Copy(audited, detail)
	return c, record(ctx, "config.change_request.create", c.ID, audited)
}

// ChangeRequestChange is a merge patch of a draft.
type ChangeRequestChange struct {
	Title, Rationale *string
	BaseVersion      *int32
	Proposed         json.RawMessage // nil: unchanged
}

// UpdateChangeRequest changes a draft or an invalid request, which returns
// to draft.
func UpdateChangeRequest(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, ch ChangeRequestChange) (ChangeRequest, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return ChangeRequest{}, err
	}
	c, err := getCR(ctx, tx, id, true)
	if err != nil {
		return ChangeRequest{}, err
	}
	if err := ifMatch(c.Version); err != nil {
		return ChangeRequest{}, err
	}
	if err := requireAuthor(c, actor); err != nil {
		return ChangeRequest{}, err
	}
	if err := requireStatus(c, "update", Draft, Invalid); err != nil {
		return ChangeRequest{}, err
	}
	title, rationale, base, proposed := c.Title, c.Rationale, c.BaseVersion, c.Proposed
	if ch.Title != nil {
		title = *ch.Title
	}
	if ch.Rationale != nil {
		rationale = *ch.Rationale
	}
	if ch.BaseVersion != nil {
		base = *ch.BaseVersion
	}
	if ch.Proposed != nil {
		proposed = ch.Proposed
	}
	if _, err := tx.Exec(ctx, `UPDATE config.change_request SET title = $2, rationale = $3, base_version = $4, proposed = $5, status = 'draft',
		version = version + 1, updated_by = $6 WHERE id = $1`, id, title, rationale, base, proposed, actor.ID); err != nil {
		return ChangeRequest{}, fmt.Errorf("configuration: update change request %s: %w", id, err)
	}
	updated, err := getCR(ctx, tx, id, false)
	if err != nil {
		return ChangeRequest{}, err
	}
	return updated, record(ctx, "config.change_request.update", id, map[string]any{"from_status": c.Status})
}

// version is a row of config.config_version.
type version struct {
	ID              uuid.UUID
	Number          int32
	Document        json.RawMessage
	Hash            []byte
	Source          string
	ChangeRequestID *uuid.UUID
	RestoredFrom    *int32
	ActivatedAt     time.Time
	ActivatedBy     uuid.UUID
	IsActive        bool
}

const versionSelect = `SELECT v.id, v.number, v.document, v.hash, v.source, v.change_request_id, v.restored_from, v.activated_at, v.activated_by,
		coalesce(a.number = v.number, false)
	FROM config.config_version v LEFT JOIN config.active_version a ON a.tenant_id = v.tenant_id`

func scanVersion(row pgx.Row) (version, error) {
	var v version
	err := row.Scan(&v.ID, &v.Number, &v.Document, &v.Hash, &v.Source, &v.ChangeRequestID, &v.RestoredFrom, &v.ActivatedAt, &v.ActivatedBy, &v.IsActive)
	return v, errs.Wrapf(err, "scan configuration version")
}

// parse reads a configuration document strictly.
func parse(doc json.RawMessage, field string) (*config.Config, error) {
	c, err := config.Parse(doc)
	if err != nil {
		return nil, problem.Unprocessable("config.validation_failed", "The configuration document cannot be read.",
			problem.FieldError{Field: field, Code: "unreadable", Message: err.Error()})
	}
	return c, nil
}

// failed reports the findings with error severity as a 422.
func failed(findings []config.Finding) error {
	var fields []problem.FieldError
	for _, f := range findings {
		if f.Severity == config.SevError {
			fields = append(fields, problem.FieldError{Field: "proposed." + f.Path, Code: "invalid", Message: f.Message})
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return problem.Unprocessable("config.validation_failed", fmt.Sprintf("The proposed configuration has %d error(s).", len(fields)), fields...)
}

// rangeTransitions checks number ranges against the base document: a range
// is never removed, and only its last number may change, upward.
func rangeTransitions(base, proposed *config.Config) []config.Finding {
	next := map[string]config.NumberRange{}
	for _, n := range proposed.NumberRanges {
		next[n.Key()] = n
	}
	var out []config.Finding
	for _, old := range base.NumberRanges {
		p := "number_ranges[" + old.Key() + "]"
		n, ok := next[old.Key()]
		switch {
		case !ok:
			out = append(out, config.Finding{Severity: config.SevError, Path: p, Message: "a number range cannot be removed"})
		case n.Prefix != old.Prefix || n.First != old.First || n.Gapless != old.Gapless:
			out = append(out, config.Finding{Severity: config.SevError, Path: p, Message: "only the last number of a number range can change"})
		case n.Last < old.Last:
			out = append(out, config.Finding{Severity: config.SevError, Path: p + ".last", Message: "the last number can only be raised"})
		}
	}
	return out
}

// rangeCompanies checks that every number range belongs to a legal entity.
func rangeCompanies(ctx context.Context, les LegalEntities, c *config.Config) ([]config.Finding, error) {
	var out []config.Finding
	for _, n := range c.NumberRanges {
		_, _, ok, err := les.LegalEntityByCode(ctx, n.Company)
		if err != nil {
			return nil, err
		}
		if !ok {
			out = append(out, config.Finding{Severity: config.SevError, Path: "number_ranges[" + n.Key() + "].company",
				Message: "no legal entity has company code " + n.Company})
		}
	}
	return out, nil
}

// SubmitChangeRequest validates a draft against its base version, computes
// its items and tier and moves it on: to awaiting_evidence when its tier
// needs evidence, to approved when it needs no approval (tier 1), else to
// pending_approval. A draft that does not validate is answered with 422
// listing the findings and stays a draft (the contract has no field for
// findings on the change request).
func SubmitChangeRequest(ctx context.Context, deps Dependencies, id uuid.UUID, ifMatch func(int32) error) (ChangeRequest, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return ChangeRequest{}, err
	}
	c, err := getCR(ctx, tx, id, true)
	if err != nil {
		return ChangeRequest{}, err
	}
	if err := ifMatch(c.Version); err != nil {
		return ChangeRequest{}, err
	}
	if err := requireStatus(c, "submit", Draft); err != nil {
		return ChangeRequest{}, err
	}
	base, err := assessableVersion(ctx, tx, c.BaseVersion)
	if err != nil {
		return ChangeRequest{}, err
	}
	proposed, err := parse(c.Proposed, "proposed")
	if err != nil {
		return ChangeRequest{}, err
	}
	if err := deps.check(ctx, tx, base, proposed); err != nil {
		return ChangeRequest{}, err
	}
	changes, _ := config.Diff(base, proposed) // never fails
	if len(changes) == 0 {
		return ChangeRequest{}, problem.Unprocessable("config.no_changes", "The proposed configuration does not change the base version.")
	}
	tier := config.MaxTier(changes)
	if err := insertItems(ctx, tx, actor.TenantID, id, changes); err != nil {
		return ChangeRequest{}, err
	}
	status := AwaitingEvidence
	var request *uuid.UUID
	if len(config.Evidence(changes)) == 0 {
		if status, request, err = route(ctx, deps, c, tier); err != nil {
			return ChangeRequest{}, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE config.change_request SET tier = $2, status = $3, workflow_request_id = $5, version = version + 1, updated_by = $4
		WHERE id = $1`, id, int(tier), status, actor.ID, request); err != nil {
		return ChangeRequest{}, fmt.Errorf("configuration: submit change request %s: %w", id, err)
	}
	updated, err := getCR(ctx, tx, id, false)
	if err != nil {
		return ChangeRequest{}, err
	}
	common := map[string]any{"change_request_id": id, "number": c.Number, "tier": int(tier), "workflow_request_id": request}
	events := []outbox.Event{
		crEvent(id, "change_request_submitted", common, map[string]any{"title": c.Title, "author_id": c.AuthorID, "base_version": c.BaseVersion,
			"status": Validating, "item_count": len(changes)}),
		crEvent(id, "change_request_validated", common, map[string]any{"outcome": status, "status": status}),
	}
	if status == Approved {
		events = append(events, crEvent(id, "change_request_approved", common, map[string]any{"base_version": c.BaseVersion, "status": Approved}))
	}
	return updated, record(ctx, "config.change_request.submit", id, map[string]any{"tier": int(tier), "items": len(changes), "status": status}, events...)
}

// insertItems stores the change request's items, numbered from 1.
func insertItems(ctx context.Context, tx pgx.Tx, tenant, id uuid.UUID, changes []config.Change) error {
	for i, ch := range changes {
		if _, err := tx.Exec(ctx, `INSERT INTO config.change_item (tenant_id, change_request_id, item_no, path, old_value, new_value, tier)
			VALUES ($1, $2, $3, $4, nullif($5, ''), nullif($6, ''), $7)`, tenant, id, i+1, ch.Path, ch.Old, ch.New, int(ch.Tier)); err != nil {
			return fmt.Errorf("configuration: insert item %d of change request %s: %w", i+1, id, err)
		}
	}
	return nil
}

func crEvent(id uuid.UUID, name string, parts ...map[string]any) outbox.Event {
	payload := map[string]any{}
	for _, p := range parts {
		for k, v := range p {
			payload[k] = v
		}
	}
	return outbox.Event{Context: "config", Name: name, AggregateType: "config.change_request", AggregateID: id, Payload: payload}
}

// WithdrawChangeRequest withdraws a request that is not yet active,
// rejected or withdrawn. Only its author may.
func WithdrawChangeRequest(ctx context.Context, deps Dependencies, id uuid.UUID, ifMatch func(int32) error, reason string) (ChangeRequest, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return ChangeRequest{}, err
	}
	c, err := getCR(ctx, tx, id, true)
	if err != nil {
		return ChangeRequest{}, err
	}
	if err := ifMatch(c.Version); err != nil {
		return ChangeRequest{}, err
	}
	if err := requireAuthor(c, actor); err != nil {
		return ChangeRequest{}, err
	}
	if err := requireStatus(c, "withdraw", Draft, Validating, Invalid, AwaitingEvidence, PendingApproval, Approved); err != nil {
		return ChangeRequest{}, err
	}
	if c.Status == PendingApproval {
		if err := deps.Approvals.Withdraw(ctx, approvals.ConfigChange, id, reason); err != nil {
			return ChangeRequest{}, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE config.change_request SET status = 'withdrawn', version = version + 1, updated_by = $2 WHERE id = $1`,
		id, actor.ID); err != nil {
		return ChangeRequest{}, fmt.Errorf("configuration: withdraw change request %s: %w", id, err)
	}
	updated, err := getCR(ctx, tx, id, false)
	if err != nil {
		return ChangeRequest{}, err
	}
	return updated, record(ctx, "config.change_request.withdraw", id, map[string]any{"from_status": c.Status, "reason": reason},
		crEvent(id, "change_request_withdrawn", map[string]any{"change_request_id": id, "number": c.Number, "previous_status": c.Status, "status": Withdrawn}))
}

// ActivateChangeRequest makes an approved change the tenant's new active
// version and applies its number ranges. The change must be based on the
// active version, so the validation done on submission still holds.
func ActivateChangeRequest(ctx context.Context, deps Dependencies, id uuid.UUID, ifMatch func(int32) error) (ChangeRequest, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return ChangeRequest{}, err
	}
	c, err := getCR(ctx, tx, id, true)
	if err != nil {
		return ChangeRequest{}, err
	}
	if err := ifMatch(c.Version); err != nil {
		return ChangeRequest{}, err
	}
	if err := requireStatus(c, "activate", Approved); err != nil {
		return ChangeRequest{}, err
	}
	current, err := activeVersion(ctx, tx)
	if err != nil {
		return ChangeRequest{}, err
	}
	if current != c.BaseVersion {
		return ChangeRequest{}, problem.Conflict("config.base_version_stale",
			fmt.Sprintf("The change is based on version %d but version %d is active; propose it again on the active version.", c.BaseVersion, current))
	}
	// Validated on submission, by this binary or a newer one: a key this one
	// does not know refuses the activation, which would drop it.
	doc, err := storedProposal(c.Proposed)
	if err != nil {
		return ChangeRequest{}, err
	}
	if err := requireRulesSimulated(c, doc); err != nil {
		return ChangeRequest{}, err
	}
	v, err := activate(ctx, tx, deps, actor, doc, &current, "change_request", &id)
	if err != nil {
		return ChangeRequest{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE config.change_request SET status = 'active', activated_version = $2, version = version + 1, updated_by = $3
		WHERE id = $1`, id, v.Number, actor.ID); err != nil {
		return ChangeRequest{}, fmt.Errorf("configuration: activate change request %s: %w", id, err)
	}
	updated, err := getCR(ctx, tx, id, false)
	if err != nil {
		return ChangeRequest{}, err
	}
	return updated, record(ctx, "config.change_request.activate", id, map[string]any{"config_version": v.Number, "tier": c.Tier})
}

// Bootstrap installs a tenant's first configuration as version 1. It runs
// when a tenant is provisioned, inside the provisioning transaction (a unit
// of work for the provisioning principal); the document must validate
// without errors.
func Bootstrap(ctx context.Context, deps Dependencies, doc json.RawMessage) (int32, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return 0, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM config.active_version)`).Scan(&exists); err != nil {
		return 0, fmt.Errorf("configuration: check for an active version: %w", err)
	}
	if exists {
		return 0, problem.Conflict("config.already_bootstrapped", "The tenant already has a configuration; change it with a change request.")
	}
	c, err := parse(doc, "document")
	if err != nil {
		return 0, err
	}
	if err := deps.check(ctx, tx, nil, c); err != nil {
		return 0, err
	}
	v, err := activate(ctx, tx, deps, actor, c, nil, "bootstrap", nil)
	return v.Number, err
}

// activate inserts the next version, makes it active, applies its number
// ranges and sections and publishes ConfigurationActivated. previous is nil
// for the bootstrap version.
func activate(ctx context.Context, tx pgx.Tx, deps Dependencies, actor reqctx.Principal, doc *config.Config, previous *int32,
	source string, changeRequest *uuid.UUID) (version, error) {
	canonical, _ := json.Marshal(doc) // Config holds only JSON-safe types
	sum := sha256.Sum256(canonical)
	number := int32(1)
	if previous != nil {
		number = *previous + 1
	}
	v, err := scanVersion(tx.QueryRow(ctx, `WITH v AS (
			INSERT INTO config.config_version (tenant_id, id, number, document, hash, source, change_request_id, activated_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING *
		) SELECT id, number, document, hash, source, change_request_id, restored_from, activated_at, activated_by, true FROM v`,
		actor.TenantID, uuid.Must(uuid.NewV7()), number, canonical, sum[:], source, changeRequest, actor.ID))
	if err != nil {
		return version{}, fmt.Errorf("configuration: insert version %d: %w", number, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO config.active_version AS a (tenant_id, number) VALUES ($1, $2)
		ON CONFLICT (tenant_id) DO UPDATE SET number = EXCLUDED.number, version = a.version + 1`, actor.TenantID, number); err != nil {
		return version{}, fmt.Errorf("configuration: activate version %d: %w", number, err)
	}
	if err := applyRanges(ctx, tx, deps.LegalEntities, actor, doc); err != nil {
		return version{}, err
	}
	for _, s := range deps.Sections {
		if err := s.Apply(ctx, doc); err != nil {
			return version{}, err
		}
	}
	return v, record(ctx, "config.version.activate", v.ID, map[string]any{"number": number, "source": source, "hash": fmt.Sprintf("%x", v.Hash)},
		outbox.Event{Context: "config", Name: "configuration_activated", AggregateType: "config.config_version", AggregateID: v.ID,
			Payload: map[string]any{"config_version_id": v.ID, "config_version": v.Number, "previous_version": previous, "source": source,
				"change_request_id": changeRequest, "restored_from": nil, "hash": fmt.Sprintf("%x", v.Hash),
				"activated_at": v.ActivatedAt.UTC().Format(time.RFC3339Nano)}})
}

// applyRanges creates the document's new number ranges and raises the
// last number of existing ones; next_value is the database's.
func applyRanges(ctx context.Context, tx pgx.Tx, les LegalEntities, actor reqctx.Principal, doc *config.Config) error {
	for _, n := range doc.NumberRanges {
		le, _, ok, err := les.LegalEntityByCode(ctx, n.Company)
		if err != nil {
			return err
		}
		if !ok { // checked on submission; legal entities are never deleted
			return fmt.Errorf("configuration: no legal entity %s for number range %s", n.Company, n.Key())
		}
		if _, err := tx.Exec(ctx, `INSERT INTO config.number_range AS r (tenant_id, id, legal_entity_id, document_type, fiscal_year, prefix,
				first_value, last_value, next_value, is_gapless, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $7, $9, $10)
			ON CONFLICT (tenant_id, legal_entity_id, document_type, fiscal_year)
			DO UPDATE SET last_value = EXCLUDED.last_value, version = r.version + 1 WHERE r.last_value <> EXCLUDED.last_value`,
			actor.TenantID, uuid.Must(uuid.NewV7()), le, n.DocumentType, n.FiscalYear, n.Prefix, n.First, n.Last, n.Gapless, actor.ID); err != nil {
			return fmt.Errorf("configuration: apply number range %s: %w", n.Key(), err)
		}
	}
	return nil
}

// record publishes the events and adds the audit entry of a change.
func record(ctx context.Context, action string, id uuid.UUID, detail map[string]any, events ...outbox.Event) error {
	objectType := "config.change_request"
	if action == "config.version.activate" {
		objectType = "config.config_version"
	}
	return audit.Record(ctx, action, objectType, id, detail, events...)
}

// Approvals moves change requests on when their approval requests
// complete: approved, rejected, or withdrawn by their author.
type Approvals struct{}

// Approved approves the change request; a person activates it.
func (Approvals) Approved(ctx context.Context, id, request uuid.UUID) error {
	return decided(ctx, id, Approved, "")
}

// Rejected rejects the change request.
func (Approvals) Rejected(ctx context.Context, id, request uuid.UUID, comment string) error {
	return decided(ctx, id, Rejected, comment)
}

// Cancelled withdraws the change request: its author withdrew the approval request.
func (Approvals) Cancelled(ctx context.Context, id, request uuid.UUID, comment string) error {
	return decided(ctx, id, Withdrawn, comment)
}

func decided(ctx context.Context, id uuid.UUID, status, comment string) error {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	c, err := getCR(ctx, tx, id, true)
	if err != nil {
		return err
	}
	if err := requireStatus(c, status, PendingApproval); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE config.change_request SET status = $2, version = version + 1, updated_by = $3 WHERE id = $1`,
		id, status, actor.ID); err != nil {
		return fmt.Errorf("configuration: decide change request %s: %w", id, err)
	}
	common := map[string]any{"change_request_id": id, "number": c.Number}
	var e outbox.Event
	switch status {
	case Approved:
		e = crEvent(id, "change_request_approved", common, map[string]any{"tier": c.Tier, "base_version": c.BaseVersion,
			"workflow_request_id": c.WorkflowRequestID, "status": Approved})
	case Rejected:
		e = crEvent(id, "change_request_rejected", common, map[string]any{"workflow_request_id": c.WorkflowRequestID,
			"previous_status": c.Status, "status": Rejected})
	default:
		e = crEvent(id, "change_request_withdrawn", common, map[string]any{"previous_status": c.Status, "status": Withdrawn})
	}
	return record(ctx, "config.change_request."+status, id, map[string]any{"from_status": c.Status, "comment": comment}, e)
}
