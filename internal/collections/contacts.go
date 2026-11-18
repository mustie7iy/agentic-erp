package collections

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Outcomes are the outcomes of a contact with a customer (T76g), an open
// enumeration of the contract: reached, the collector spoke to the
// customer; not_reached, nobody answered; left_message, a message was
// left.
var Outcomes = []string{"reached", "not_reached", "left_message"}

// Contact is a row of collections.customer_contact (ADR-0065 As built,
// T76g): a collector's contact with a customer of a company at
// ContactedAt, with whom (ContactName), its Outcome and Note, and the
// date the customer is to be worked again (Resubmission). CustomerNumber
// is the customer's when it was recorded; CreatedBy is the collector. A
// contact is not changed: Version stays 1.
type Contact struct {
	ID             uuid.UUID
	LegalEntityID  uuid.UUID
	CustomerID     uuid.UUID
	CustomerNumber string
	ContactedAt    time.Time
	ContactName    *string
	Outcome        string
	Note           *string
	Resubmission   *time.Time
	Version        int32
	CreatedAt      time.Time
	CreatedBy      uuid.UUID
	UpdatedAt      time.Time
	UpdatedBy      uuid.UUID
}

const contactSelect = `SELECT c.id, c.legal_entity_id, c.customer_id, c.customer_number, c.contacted_at, c.contact_name, c.outcome, c.note,
		c.resubmission_date, c.version, c.created_at, c.created_by, c.updated_at, c.updated_by
	FROM collections.customer_contact c`

func scanContact(row pgx.Row) (Contact, error) {
	var c Contact
	err := row.Scan(&c.ID, &c.LegalEntityID, &c.CustomerID, &c.CustomerNumber, &c.ContactedAt, &c.ContactName, &c.Outcome, &c.Note, &c.Resubmission,
		&c.Version, &c.CreatedAt, &c.CreatedBy, &c.UpdatedAt, &c.UpdatedBy)
	c.ContactedAt = c.ContactedAt.UTC() // an instant, given as it was sent: in UTC, whatever the session's zone
	return c, errs.Wrapf(err, "scan customer contact")
}

// GetContact returns a contact of a legal entity the caller may act in;
// another company's is not found, so ids cannot be probed (04 §4.9).
func GetContact(ctx context.Context, id uuid.UUID) (Contact, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Contact{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Contact{}, fmt.Errorf("collections: company access for customer contact %s: %w", id, err)
	}
	c, err := scanContact(tx.QueryRow(ctx, contactSelect+` WHERE c.id = $1`, id))
	if err := authz.Visible(err, access, c.LegalEntityID, problem.NotFoundOf("customer contact"), "collections: read customer contact %s", id); err != nil {
		return Contact{}, err
	}
	return c, nil
}

// ContactInput is a contact as a client records it.
type ContactInput struct {
	LegalEntityID uuid.UUID
	CustomerID    uuid.UUID
	ContactedAt   time.Time
	ContactName   *string
	Outcome       string
	Note          *string
	Resubmission  *time.Time
}

// RecordContact records a collector's contact with a customer of a
// company the caller may act in (ADR-0065 As built, T76g): the customer
// is a business partner of the tenant; the contact was not after now by
// the endpoints' clock; the outcome is one of Outcomes; the texts are
// more than white space; a resubmission date is after the company's
// today and at most LastKeyDate. The audit record is written in the
// request's transaction; nothing is posted or sent.
func (e *Endpoints) RecordContact(ctx context.Context, in ContactInput) (Contact, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Contact{}, err
	}
	if err := checkContact(in, clock.Now(e.Now)); err != nil {
		return Contact{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Contact{}, fmt.Errorf("collections: company access: %w", err)
	}
	le, err := tenancy.Accessible(ctx, e.LegalEntities, access, in.LegalEntityID, "legal_entity_id")
	if err != nil {
		return Contact{}, fmt.Errorf("collections: legal entity of a customer contact: %w", err)
	}
	if err := e.resubmission(ctx, le, in.Resubmission); err != nil {
		return Contact{}, err
	}
	number, err := e.namedCustomer(ctx, in.CustomerID)
	if err != nil {
		return Contact{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO collections.customer_contact (tenant_id, id, legal_entity_id, customer_id, customer_number, contacted_at,
			contact_name, outcome, note, resubmission_date, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11)`, p.TenantID, id, le.ID, in.CustomerID, number, in.ContactedAt, in.ContactName,
		in.Outcome, in.Note, in.Resubmission, p.ID); err != nil {
		return Contact{}, fmt.Errorf("collections: record a contact with customer %s: %w", in.CustomerID, err)
	}
	c, err := GetContact(ctx, id)
	if err != nil {
		return Contact{}, err
	}
	return c, recordContact(ctx, c)
}

// checkContact refuses a contact after now, an outcome the server does
// not know and a text of white space alone, each naming its field.
func checkContact(in ContactInput, now time.Time) error {
	if in.ContactedAt.After(now) {
		return problem.Invalid("collections.invalid_contact_time", "contacted_at", "A contact is recorded once it has happened, at "+
			now.UTC().Format(time.RFC3339)+" at the latest.")
	}
	if !slices.Contains(Outcomes, in.Outcome) {
		return problem.Invalid("collections.unknown_outcome", "outcome", fmt.Sprintf("%q is no outcome of a contact; the outcomes are %v.", in.Outcome,
			Outcomes))
	}
	for _, f := range []struct {
		field string
		text  *string
	}{{"contact_name", in.ContactName}, {"note", in.Note}} {
		if f.text != nil {
			if err := problem.RequireText(f.field, *f.text); err != nil {
				return err
			}
		}
	}
	return nil
}

// resubmission refuses a resubmission date that is not after the
// company's today, which would take the customer off no worklist from
// now on, or after LastKeyDate, the latest key date.
func (e *Endpoints) resubmission(ctx context.Context, le tenancy.Entity, date *time.Time) error {
	if date == nil {
		return nil
	}
	today, err := e.today(ctx, le)
	if err != nil {
		return err
	}
	if !date.After(today.Date) || date.After(LastKeyDate) {
		return problem.Invalid("collections.invalid_resubmission_date", "resubmission_date", fmt.Sprintf("A customer is resubmitted on a date "+
			"after company %s's today, %s (%s), up to %s.", le.Code, today.Date.Format(time.DateOnly), today.Zone, LastKeyDate.Format(time.DateOnly)))
	}
	return nil
}

// recordContact adds the audit record of a contact recorded (ADR-0011).
// Its detail says whether the contact names a person and has a note but
// holds neither: they are personal data (records.Inventory), which a
// deletion run blanks on the contact (ADR-0079) and could not blank in
// the insert-only audit trail (ADR-0026: identifiers, not names). A
// contact publishes no event: it changes nothing outside the context.
// Without events audit.Record fails only outside a unit of work, which
// reqctx.Request refused before.
func recordContact(ctx context.Context, c Contact) error {
	detail := map[string]any{"legal_entity_id": c.LegalEntityID, "customer_id": c.CustomerID, "contacted_at": c.ContactedAt, "outcome": c.Outcome,
		"has_contact_name": c.ContactName != nil, "has_note": c.Note != nil, "resubmission_date": nil}
	if c.Resubmission != nil {
		detail["resubmission_date"] = c.Resubmission.Format(time.DateOnly)
	}
	return audit.Record(ctx, "collections.customer_contact.record", "collections.customer_contact", c.ID, detail)
}

// lastContact is a customer's last contact as the worklist shows it.
type lastContact struct {
	At           time.Time
	Outcome      string
	Resubmission *time.Time
}

// lastContacts reads the customers' last contacts in the company (their
// latest by contacted_at, over the index customer_contact_latest), by
// customer; a customer never contacted is absent. With customers nil it
// reads those of the customers with a contact resubmitted after the key
// date (customer_contact_resubmission), which the worklist leaves out
// when that is their last.
func lastContacts(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID, customers []uuid.UUID, after time.Time) (map[uuid.UUID]lastContact, error) {
	of, arg := `c.customer_id = ANY($2)`, any(customers)
	if customers == nil {
		of, arg = `c.customer_id IN (SELECT r.customer_id FROM collections.customer_contact r WHERE r.legal_entity_id = $1 AND r.resubmission_date > $2)`, after
	}
	// Read into a map, so the rows' order across customers does not matter.
	rows, _ := tx.Query(ctx, `SELECT DISTINCT ON (c.customer_id) c.customer_id, c.contacted_at, c.outcome, c.resubmission_date
		FROM collections.customer_contact c WHERE c.legal_entity_id = $1 AND `+of+`
		ORDER BY c.customer_id, c.contacted_at DESC, c.id DESC`, legalEntity, arg)
	out := map[uuid.UUID]lastContact{}
	var customer uuid.UUID
	var c lastContact
	_, err := pgx.ForEachRow(rows, []any{&customer, &c.At, &c.Outcome, &c.Resubmission}, func() error {
		c.At = c.At.UTC()
		out[customer] = c
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("collections: read the last contacts with the customers of company %s: %w", legalEntity, err)
	}
	return out, nil
}

// heldUntil reports whether the last contact keeps its customer off a
// worklist at the key date: its resubmission date is after it.
func (c lastContact) heldUntil(keyDate time.Time) bool {
	return c.Resubmission != nil && c.Resubmission.After(keyDate)
}
