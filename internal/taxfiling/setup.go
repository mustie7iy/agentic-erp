package taxfiling

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Setup is the configuration section of tax filing: it keeps the
// document's tax registrations in tax.registration. One the document no
// longer lists becomes inactive, never deleted, since returns will name it
// (T61i).
type Setup struct {
	LegalEntities interface {
		EntityByCode(ctx context.Context, code string) (tenancy.Entity, bool, error)
	}
	Keys *fieldcrypt.Keyring
}

// wanted is a registration of the document with its companies' legal
// entities, the members in ascending order.
type wanted struct {
	path    string
	reg     config.TaxRegistration
	le      uuid.UUID
	members []uuid.UUID
	first   time.Time
	last    *time.Time
}

// stored is what a registration's periods and returns follow from, as
// tax.registration has it.
type stored struct {
	le                          uuid.UUID
	members                     []uuid.UUID
	authority, returnType, freq string
	first                       time.Time
	sealed                      []byte
}

// Check reports each registration whose fixed settings the proposed
// document changes. A registration of a company or member without a legal
// entity is left to the ledger's section, which reports the company.
func (s Setup) Check(ctx context.Context, _, proposed *config.Config) ([]config.Finding, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	regs, err := s.resolve(ctx, proposed, false)
	if err != nil {
		return nil, err
	}
	changed, err := s.fixed(ctx, tx, p.TenantID, regs, false)
	if err != nil {
		return nil, err
	}
	out := make([]config.Finding, len(changed))
	for i, path := range changed {
		out[i] = config.Finding{Severity: config.SevError, Path: path, Message: "a registration's company, group members, authority, number, return type, " +
			"frequency and first period never change under its code: they are what its periods and returns follow from; give the changed registration " +
			"a new code, and end this one with its last_period_end"}
	}
	return out, nil
}

// Apply makes the tenant's registrations the document's, refusing one
// whose fixed settings changed since it was checked (409
// tax.registration_fixed): the registrations are read FOR UPDATE first.
func (s Setup) Apply(ctx context.Context, doc *config.Config) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	regs, err := s.resolve(ctx, doc, true)
	if err != nil {
		return err
	}
	changed, err := s.fixed(ctx, tx, p.TenantID, regs, true)
	if err != nil {
		return err
	}
	if len(changed) > 0 {
		return registrationFixed("The configuration changes what tax registrations' periods and returns follow from: " + strings.Join(changed, ", ") + ".")
	}
	codes := make([]string, len(regs))
	for i, w := range regs {
		codes[i] = w.reg.Code
	}
	if _, err := tx.Exec(ctx, `UPDATE tax.registration SET is_active = false, version = version + 1, updated_by = $2
		WHERE is_active AND code <> ALL($1)`, codes, p.ID); err != nil {
		return fmt.Errorf("taxfiling: deactivate the unlisted tax registrations: %w", err)
	}
	for _, w := range regs {
		if err := s.upsert(ctx, tx, p, w); err != nil {
			return fmt.Errorf("taxfiling: apply tax registration %s: %w", w.reg.Code, err)
		}
	}
	return nil
}

// resolve finds the legal entities and dates of the document's
// registrations. A company without a legal entity, or a date that does not
// parse, is skipped while checking (strict false), since the ledger's
// section or the document's validation reports it, and is an error while
// applying, since the document was checked.
func (s Setup) resolve(ctx context.Context, doc *config.Config, strict bool) ([]wanted, error) {
	var out []wanted
	for _, co := range doc.Companies {
		for _, r := range co.TaxRegistrations {
			w := wanted{path: fmt.Sprintf("companies[%s].tax_registrations[%s]", co.Code, r.Code), reg: r}
			ok, err := s.entities(ctx, &w, co.Code)
			if err != nil {
				return nil, err
			}
			if !ok {
				if strict {
					return nil, fmt.Errorf("taxfiling: tax registration %s: a company it covers has no legal entity", r.Code)
				}
				continue
			}
			if err := w.dates(); err != nil {
				if strict {
					return nil, err
				}
				continue // the document's validation names the date
			}
			out = append(out, w)
		}
	}
	return out, nil
}

// entities sets the registrant's and members' legal entities; ok is false
// when one has none.
func (s Setup) entities(ctx context.Context, w *wanted, registrant string) (bool, error) {
	for i, code := range append([]string{registrant}, w.reg.GroupMembers...) {
		le, ok, err := s.LegalEntities.EntityByCode(ctx, code)
		if err != nil || !ok {
			return false, errs.Wrapf(err, "taxfiling: legal entity %s", code)
		}
		if i == 0 {
			w.le = le.ID
			continue
		}
		w.members = append(w.members, le.ID)
	}
	slices.SortFunc(w.members, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return true, nil
}

// dates parses the registration's first and last day, which the
// document's validation has checked.
func (w *wanted) dates() error {
	first, err := time.Parse(time.DateOnly, w.reg.FirstPeriodStart)
	if err != nil {
		return fmt.Errorf("taxfiling: tax registration %s: first period start: %w", w.reg.Code, err)
	}
	w.first = first
	if w.reg.LastPeriodEnd == "" {
		return nil
	}
	last, err := time.Parse(time.DateOnly, w.reg.LastPeriodEnd)
	if err != nil {
		return fmt.Errorf("taxfiling: tax registration %s: last period end: %w", w.reg.Code, err)
	}
	w.last = &last
	return nil
}

// fixed lists the paths of the fixed settings regs change from what
// tax.registration has, read FOR UPDATE when lock is set.
func (s Setup) fixed(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, regs []wanted, lock bool) ([]string, error) {
	have, err := readStored(ctx, tx, regs, lock)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, w := range regs {
		st, ok := have[w.reg.Code]
		if !ok {
			continue
		}
		number, err := s.Keys.Open(ctx, tenant, st.sealed)
		if err != nil {
			return nil, fmt.Errorf("taxfiling: tax registration %s: open the number: %w", w.reg.Code, err)
		}
		for field, same := range map[string]bool{
			"":                    st.le == w.le,
			".group_members":      slices.Equal(st.members, w.members),
			".authority":          st.authority == w.reg.Authority,
			".number":             number == fieldcrypt.Normalize(w.reg.Number),
			".return_type":        st.returnType == w.reg.ReturnType,
			".frequency":          st.freq == w.reg.Frequency,
			".first_period_start": st.first.Equal(w.first),
		} {
			if !same {
				out = append(out, w.path+field)
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

// readStored reads the stored registrations of regs' codes.
func readStored(ctx context.Context, tx pgx.Tx, regs []wanted, lock bool) (map[string]stored, error) {
	codes := make([]string, len(regs))
	for i, w := range regs {
		codes[i] = w.reg.Code
	}
	q := `SELECT code, legal_entity_id, group_member_ids, authority, return_type, frequency, first_period_start, number_cipher
		FROM tax.registration WHERE code = ANY($1) ORDER BY code`
	if lock {
		q += ` FOR UPDATE`
	}
	rows, _ := tx.Query(ctx, q, codes)
	out := map[string]stored{}
	var code string
	var st stored
	_, err := pgx.ForEachRow(rows, []any{&code, &st.le, &st.members, &st.authority, &st.returnType, &st.freq, &st.first, &st.sealed}, func() error {
		slices.SortFunc(st.members, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
		out[code] = st
		st = stored{}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("taxfiling: read tax registrations: %w", err)
	}
	return out, nil
}

// upsert creates the registration, its number sealed, or updates its end
// and reactivates it; its fixed settings are the stored ones (fixed).
func (s Setup) upsert(ctx context.Context, tx pgx.Tx, p reqctx.Principal, w wanted) error {
	sealed, err := s.Keys.Seal(ctx, p.TenantID, w.reg.Number)
	if err != nil {
		return err
	}
	print, err := s.Keys.Fingerprint(ctx, p.TenantID, w.reg.Number)
	if err != nil {
		return err
	}
	members := w.members
	if members == nil {
		members = []uuid.UUID{}
	}
	_, err = tx.Exec(ctx, `INSERT INTO tax.registration AS r (tenant_id, id, code, legal_entity_id, group_member_ids, authority, number_cipher,
			number_fingerprint, number_last4, return_type, frequency, first_period_start, last_period_end, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $14)
		ON CONFLICT (tenant_id, code) DO UPDATE SET last_period_end = EXCLUDED.last_period_end, is_active = true, version = r.version + 1,
			updated_by = EXCLUDED.updated_by
		WHERE (r.last_period_end, r.is_active) IS DISTINCT FROM (EXCLUDED.last_period_end, true)`,
		p.TenantID, uuid.Must(uuid.NewV7()), w.reg.Code, w.le, members, w.reg.Authority, sealed, print, fieldcrypt.Last4(w.reg.Number),
		w.reg.ReturnType, w.reg.Frequency, w.first, w.last, p.ID)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}
