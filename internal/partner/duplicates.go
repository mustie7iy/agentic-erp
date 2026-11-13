package partner

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// What a possible duplicate matched (T89a; 03-data-model §3.6).
const (
	MatchTaxNumber      = "tax_registration_number"
	MatchNameAndCountry = "name_and_country"
)

// MaxPossibleDuplicates bounds the partners a change lists: a common name
// may match many, and the approver needs the first few to judge.
const MaxPossibleDuplicates = 20

// Duplicate is an existing business partner a new one may duplicate: by
// id, number and name, with what matched (MatchTaxNumber,
// MatchNameAndCountry), in that order.
type Duplicate struct {
	PartnerID uuid.UUID `json:"partner_id"`
	Number    string    `json:"number"`
	Name      string    `json:"name"`
	Matched   []string  `json:"matched"`
}

// NormalizedName is a partner's name as duplicates are matched by it:
// white space trimmed and each run of it one space, in capitals. The
// migration's quality gate flags vendors by it, and the partner context's
// duplicate check finds partners by it, through the database's
// partner.normalized_name, which computes the same (migration 000309).
func NormalizedName(name string) string {
	return strings.ToUpper(strings.Join(strings.Fields(name), " "))
}

// possibleDuplicates are the existing partners a new one named name in
// country, with the tax identifiers taxes, may duplicate (T89a): those
// with one of its tax registration numbers (the same scheme and value)
// or its normalized name and country, whatever their roles, a blocked one
// included, by number, at most MaxPossibleDuplicates. A partner whose
// create change was rejected or cancelled never existed and is left out.
// The tax identifiers' advisory locks are taken first, one per scheme and
// value in their order, so of two partners with the same number created
// at once the second waits and then finds the first (ADR-0008).
func possibleDuplicates(ctx context.Context, tx pgx.Tx, name, country string, taxes []sealedTax) ([]Duplicate, error) {
	schemes, prints := make([]string, len(taxes)), make([]string, len(taxes))
	keys := make([]string, len(taxes))
	for i, t := range taxes {
		schemes[i], prints[i], keys[i] = t.Scheme, t.ValueFingerprint, t.Scheme+":"+t.ValueFingerprint
	}
	slices.Sort(keys)
	for _, k := range keys {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('partner.tax_identifier:' || platform.require_tenant() || ':' || $1, 0))`,
			k); err != nil {
			return nil, fmt.Errorf("partner: lock tax registration number %s: %w", k, err)
		}
	}
	rows, _ := tx.Query(ctx, `SELECT p.id, p.number, p.name,
			EXISTS (SELECT FROM partner.tax_identifier t JOIN unnest($3::text[], $4::text[]) AS x(scheme, print)
				ON t.scheme = x.scheme AND t.value_fingerprint = x.print WHERE t.tenant_id = p.tenant_id AND t.partner_id = p.id),
			p.country_code = $2 AND partner.normalized_name(p.name) = $1
		FROM partner.business_partner p
		WHERE ((p.country_code = $2 AND partner.normalized_name(p.name) = $1)
				OR p.id IN (SELECT t.partner_id FROM partner.tax_identifier t JOIN unnest($3::text[], $4::text[]) AS x(scheme, print)
					ON t.scheme = x.scheme AND t.value_fingerprint = x.print))
			AND (p.status <> 'pending' OR EXISTS (SELECT FROM partner.partner_change c
				WHERE c.tenant_id = p.tenant_id AND c.partner_id = p.id AND c.kind = 'create' AND c.status = 'pending'))
		ORDER BY p.number, p.id LIMIT $5`, NormalizedName(name), country, schemes, prints, MaxPossibleDuplicates)
	var d Duplicate
	var tax, named bool
	out := []Duplicate{}
	_, err := pgx.ForEachRow(rows, []any{&d.PartnerID, &d.Number, &d.Name, &tax, &named}, func() error {
		d.Matched = nil
		if tax {
			d.Matched = append(d.Matched, MatchTaxNumber)
		}
		if named {
			d.Matched = append(d.Matched, MatchNameAndCountry)
		}
		out = append(out, d)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("partner: possible duplicates of %s: %w", name, err)
	}
	return out, nil
}

// requireConfirmed refuses a new partner whose tax registration number
// another partner already has, unless its creator confirms it (409
// partner.duplicate_tax_number, naming those partners); a match on name
// and country is only listed.
func requireConfirmed(dups []Duplicate, confirmed bool) error {
	var taken []string
	for _, d := range dups {
		if slices.Contains(d.Matched, MatchTaxNumber) {
			taken = append(taken, d.Number+" "+d.Name)
		}
	}
	if len(taken) == 0 || confirmed {
		return nil
	}
	return problem.Conflict("partner.duplicate_tax_number", "A tax registration number of the new partner is in use by "+strings.Join(taken, ", ")+
		"; create it with confirm_duplicate true if it is another partner, and its approver sees the duplicates.")
}
