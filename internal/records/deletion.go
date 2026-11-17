package records

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	partnerctx "github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Deletion runs (ADR-0079 §12, T100c): the business partners whose
// personal data has been blocked longer than the retention rule for
// personal data of every company they have data in are queued in the
// request's transaction and blanked by the Deleter, one per transaction,
// through the partner context's port. Ledger documents and audit events
// stay as they are.

// Privacy is the partner context's personal data (partner.Privacy): the
// partners whose personal data is blocked, and blanking one.
type Privacy interface {
	Blocked(ctx context.Context) ([]partnerctx.BlockedSubject, error)
	Blank(ctx context.Context, id uuid.UUID, blockedAt time.Time) (string, error)
}

// Statuses of a deletion run's subject.
const (
	SubjectPending = "pending"
	SubjectBlanked = "blanked"
	SubjectSkipped = "skipped"
)

// Subject is a business partner a deletion run blanks: by id and number,
// when its block began, the longest rule it is held to and the day that
// rule's years ended; in a run, its status, when it was settled and, when
// skipped, why (the partner context's reason; none on a subject skipped
// before reasons were kept).
type Subject struct {
	PartnerID  uuid.UUID
	Number     string
	BlockedAt  time.Time
	Years      int16
	Ends       time.Time
	Status     string
	SettledAt  *time.Time
	SkipReason *string
}

// DeletionRun is a deletion run with its subjects by number.
type DeletionRun struct {
	ID                   uuid.UUID
	Status               string
	ConfigVersion        int32
	Count                int32
	Pending              int32
	Blanked              int32
	Skipped              int32
	FailureCode          *string
	CompletedAt          *time.Time
	RequestedBy          uuid.UUID
	RequestedByName      string
	CreatedAt, UpdatedAt time.Time
}

func deletionRunNotFound() error { return problem.NotFoundOf("deletion run") }

// allCompanies refuses a caller who may not act in every company: what
// is read or blanked is the whole tenant's.
func allCompanies(ctx context.Context, what string) error {
	access, err := authz.Companies(ctx)
	if err != nil {
		return fmt.Errorf("records: company access for %s: %w", what, err)
	}
	if !access.All {
		return problem.Forbidden("records.all_companies_required",
			"The personal data of business partners is the whole tenant's, every company's; the caller needs access to all companies.")
	}
	return nil
}

// Eligible are the subjects a deletion run started now blanks, by number,
// and the active configuration version their rules were read from: each
// partner whose personal data is blocked and not deleted, that has data
// in at least one company, and whose block has lasted, in every such
// company, the whole years of its rule for personal data, counted from
// the block's day on the company's date. A company without a rule keeps
// its partners.
func (e *Endpoints) Eligible(ctx context.Context) ([]Subject, int32, error) {
	if _, _, err := reqctx.Request(ctx); err != nil {
		return nil, 0, err
	}
	if err := allCompanies(ctx, "a deletion run"); err != nil {
		return nil, 0, err
	}
	doc, version, err := e.Settings.Document(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("records: configuration of a deletion run: %w", err)
	}
	blocked, err := e.Privacy.Blocked(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("records: partners whose personal data is blocked: %w", err)
	}
	codes := map[uuid.UUID]string{}
	now := clock.Now(e.Now)
	out := []Subject{}
	for _, b := range blocked {
		s, ok, err := e.eligible(ctx, doc, codes, now, b)
		if err != nil {
			return nil, 0, err
		}
		if ok {
			out = append(out, s)
		}
	}
	return out, version, nil
}

// eligible is the partner as a subject, and whether its block has lasted
// the rule of every company it has data in; codes caches the companies'
// codes.
func (e *Endpoints) eligible(ctx context.Context, doc *config.Config, codes map[uuid.UUID]string, now time.Time, b partnerctx.BlockedSubject) (Subject, bool, error) {
	s := Subject{PartnerID: b.ID, Number: b.Number, BlockedAt: b.BlockedAt, Years: -1}
	for _, le := range b.Companies {
		code, err := e.companyCode(ctx, codes, le)
		if err != nil {
			return s, false, err
		}
		co := doc.Company(code)
		if co == nil || co.RetentionFor(config.RetentionPersonalData) == nil {
			return s, false, nil
		}
		years := *co.RetentionFor(config.RetentionPersonalData).Years
		blockDay, err := doc.CompanyDay(code, b.BlockedAt)
		if err != nil {
			return s, false, fmt.Errorf("records: date of company %s: %w", code, err)
		}
		ends := config.RetentionEnds(blockDay.Date, years)
		if clock.DayIn(now, blockDay.Zone).Before(ends) {
			return s, false, nil
		}
		s.Years, s.Ends = max(s.Years, int16(years)), clock.Later(s.Ends, ends) // #nosec G115 -- years are 0 to 99 (config validation)
	}
	return s, len(b.Companies) > 0, nil
}

// companyCode is the legal entity's company code, read once.
func (e *Endpoints) companyCode(ctx context.Context, codes map[uuid.UUID]string, le uuid.UUID) (string, error) {
	if code, ok := codes[le]; ok {
		return code, nil
	}
	entity, found, err := e.LegalEntities.Entity(ctx, le)
	if err != nil {
		return "", fmt.Errorf("records: company %s of a blocked partner: %w", le, err)
	}
	if !found {
		return "", fmt.Errorf("records: company %s of a blocked partner is not held by the tenancy context", le)
	}
	codes[le] = entity.Code
	return entity.Code, nil
}

// RequestDeletion queues a deletion run of the subjects eligible now and
// reports whether it is new: a run queued already is answered instead.
// None eligible is refused.
func (e *Endpoints) RequestDeletion(ctx context.Context, reason string) (DeletionRun, bool, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return DeletionRun{}, false, err
	}
	if err := allCompanies(ctx, "a deletion run"); err != nil { // before a queued run is answered, as reading one is refused
		return DeletionRun{}, false, err
	}
	if r, found, err := e.queuedDeletion(ctx, tx); err != nil || found {
		return r, false, err
	}
	subjects, version, err := e.Eligible(ctx)
	if err != nil {
		return DeletionRun{}, false, err
	}
	if len(subjects) == 0 {
		return DeletionRun{}, false, problem.Unprocessable("records.nothing_to_delete", "No business partner's personal data has been blocked "+
			"longer than the retention rules for personal data of its companies; see GET /v1/personal-data-deletion-preview.")
	}
	var id uuid.UUID
	err = tx.QueryRow(ctx, `INSERT INTO records.deletion_run (tenant_id, id, config_version, subject_count, requested_by) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id) WHERE status = 'queued' DO NOTHING RETURNING id`, p.TenantID, uuid.Must(uuid.NewV7()), version, len(subjects), p.ID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) { // a run queued at the same moment answers
		r, _, err := e.queuedDeletion(ctx, tx)
		return r, false, err
	}
	if err != nil {
		return DeletionRun{}, false, fmt.Errorf("records: queue a deletion run: %w", err)
	}
	if err := insertSubjects(ctx, tx, p.TenantID, id, subjects); err != nil {
		return DeletionRun{}, false, err
	}
	r, err := e.deletionRun(ctx, tx, id)
	if err != nil {
		return DeletionRun{}, false, err
	}
	return r, true, audit.Record(ctx, "records.deletion_run.request", "records.deletion_run", id, map[string]any{"subject_count": len(subjects),
		"config_version": version, "reason": reason})
}

// insertSubjects writes the run's subjects in one statement.
func insertSubjects(ctx context.Context, tx pgx.Tx, tenant, run uuid.UUID, subjects []Subject) error {
	ids, numbers, at, years, ends := make([]uuid.UUID, len(subjects)), make([]string, len(subjects)), make([]time.Time, len(subjects)),
		make([]int16, len(subjects)), make([]time.Time, len(subjects))
	for i, s := range subjects {
		ids[i], numbers[i], at[i], years[i], ends[i] = s.PartnerID, s.Number, s.BlockedAt, s.Years, s.Ends
	}
	if _, err := tx.Exec(ctx, `INSERT INTO records.deletion_run_subject (tenant_id, run_id, partner_id, partner_number, blocked_at, retention_years, retention_ends)
		SELECT $1, $2, x.id, x.number, x.at, x.years, x.ends FROM unnest($3::uuid[], $4::text[], $5::timestamptz[], $6::smallint[], $7::date[]) AS x(id, number, at, years, ends)`,
		tenant, run, ids, numbers, at, years, ends); err != nil {
		return fmt.Errorf("records: subjects of deletion run %s: %w", run, err)
	}
	return nil
}

const deletionRunSelect = `SELECT id, status, config_version, subject_count, failure_code, completed_at, requested_by, created_at, updated_at FROM records.deletion_run`

func scanDeletionRun(row pgx.Row) (DeletionRun, error) {
	var r DeletionRun
	err := row.Scan(&r.ID, &r.Status, &r.ConfigVersion, &r.Count, &r.FailureCode, &r.CompletedAt, &r.RequestedBy, &r.CreatedAt, &r.UpdatedAt)
	return r, errs.Wrapf(err, "records: scan deletion run")
}

// queuedDeletion is the tenant's queued deletion run, if there is one.
func (e *Endpoints) queuedDeletion(ctx context.Context, tx pgx.Tx) (DeletionRun, bool, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM records.deletion_run WHERE status = 'queued'`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeletionRun{}, false, nil
	}
	if err != nil {
		return DeletionRun{}, false, fmt.Errorf("records: the queued deletion run: %w", err)
	}
	r, err := e.deletionRun(ctx, tx, id)
	return r, err == nil, err
}

// DeletionRun is a deletion run with its subjects, to a caller who may act
// in every company.
func (e *Endpoints) DeletionRun(ctx context.Context, id uuid.UUID) (DeletionRun, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return DeletionRun{}, err
	}
	if err := allCompanies(ctx, "a deletion run"); err != nil {
		return DeletionRun{}, err
	}
	return e.deletionRun(ctx, tx, id)
}

// deletionRun reads a run, its subjects' counts by status and its
// requester's name.
func (e *Endpoints) deletionRun(ctx context.Context, tx pgx.Tx, id uuid.UUID) (DeletionRun, error) {
	r, err := scanDeletionRun(tx.QueryRow(ctx, deletionRunSelect+` WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, deletionRunNotFound()
	}
	if err != nil {
		return r, fmt.Errorf("records: deletion run %s: %w", id, err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status = 'pending'), count(*) FILTER (WHERE status = 'blanked'),
			count(*) FILTER (WHERE status = 'skipped') FROM records.deletion_run_subject WHERE run_id = $1`, id).Scan(&r.Pending, &r.Blanked, &r.Skipped); err != nil {
		return r, fmt.Errorf("records: subjects of deletion run %s: %w", id, err)
	}
	names, err := e.People.PersonNames(ctx, []uuid.UUID{r.RequestedBy})
	if err != nil {
		return r, fmt.Errorf("records: requester of deletion run %s: %w", id, err)
	}
	r.RequestedByName = names[r.RequestedBy]
	return r, nil
}

var subjectSort = pagination.Sort{Default: "partner_number", Fields: []pagination.Field{{Name: "partner_number", Column: "partner_number", Type: "text"}}}

// Subjects lists a page of a run's subjects by partner number, of one
// status or all, and the next page's token ("" on the last), to a caller
// who may act in every company.
func (e *Endpoints) Subjects(ctx context.Context, run uuid.UUID, status string, page pagination.Request) ([]Subject, string, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, "", err
	}
	if err := allCompanies(ctx, "a deletion run"); err != nil {
		return nil, "", err
	}
	ks, err := e.Pages.Keyset(subjectSort, "listPersonalDataDeletionSubjects|"+run.String()+"|"+status, page)
	if err != nil {
		return nil, "", err
	}
	var found bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM records.deletion_run WHERE id = $1)`, run).Scan(&found); err != nil {
		return nil, "", fmt.Errorf("records: deletion run %s: %w", run, err)
	}
	if !found {
		return nil, "", deletionRunNotFound()
	}
	// The subject's partner is its id in the keyset's tie-break.
	q, args := ks.SQL(`SELECT id, partner_number, blocked_at, retention_years, retention_ends, status, settled_at, skip_reason FROM (SELECT partner_id AS id,
		partner_number, blocked_at, retention_years, retention_ends, status, settled_at, skip_reason FROM records.deletion_run_subject WHERE run_id = $1) s
		WHERE ($2 = '' OR status = $2)`,
		[]any{run, status})
	rows, _ := tx.Query(ctx, q, args...)
	ss, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Subject, error) {
		var s Subject
		return s, row.Scan(&s.PartnerID, &s.Number, &s.BlockedAt, &s.Years, &s.Ends, &s.Status, &s.SettledAt, &s.SkipReason)
	})
	if err != nil {
		return nil, "", fmt.Errorf("records: subjects of deletion run %s: %w", run, err)
	}
	ss, next := pagination.Page(ks, ss, subjectKey)
	return ss, next, nil
}

// subjectKey is a subject's place in its order: its partner's number and
// id.
func subjectKey(s Subject) (string, uuid.UUID) { return s.Number, s.PartnerID }

// Preview is a page of the subjects Eligible lists, by partner number,
// their count on every page, the configuration version, and the next
// page's token ("" on the last).
func (e *Endpoints) Preview(ctx context.Context, page pagination.Request) ([]Subject, int, int32, string, error) {
	ks, err := e.Pages.Keyset(subjectSort, "getPersonalDataDeletionPreview", page)
	if err != nil {
		return nil, 0, 0, "", err
	}
	all, version, err := e.Eligible(ctx)
	if err != nil {
		return nil, 0, 0, "", err
	}
	items, next := pagination.InMemory(ks, all, subjectKey)
	return items, len(all), version, next, nil
}
