package materials

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/langtag"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Descriptions by language (S1a2; ADR-0082 As built S1a2): a material's
// description is its default one, in its DescriptionLanguage, which does
// not change (material_language_fixed); it may have one further
// description in each other language (SAP MAKT, from memory). A language
// is a BCP 47 tag in its canonical form (langtag). A read asks for a
// language and is answered its description there, else the default.

// DefaultLanguage is the default description's language when a request
// names none, and the one materials created before S1a2 were described
// in (000480).
const DefaultLanguage = "en"

// Description is a material's description in a language, kept as given.
type Description struct {
	Language string `json:"language"`
	Text     string `json:"description"`
}

// AllDescriptions are the material's descriptions in every language it
// has: the default first, then the others by language.
func (m Material) AllDescriptions() []Description {
	return append([]Description{{Language: m.DescriptionLanguage, Text: m.Description}}, m.Descriptions...)
}

// DescriptionIn is the material's description in the language, found by
// RFC 4647 lookup among the languages it has (langtag.Lookup: ar-AE finds
// ar), else its default description; "" asks for the default.
func (m Material) DescriptionIn(language string) Description {
	all := m.AllDescriptions()
	have := make([]string, len(all))
	for i, d := range all {
		have[i] = d.Language
	}
	if l, ok := langtag.Lookup(language, have); ok {
		return all[slices.Index(have, l)]
	}
	return all[0]
}

// checkLanguage refuses a language that is no tag a description is kept
// by (422 materials.unknown_language naming the field): ill-formed, a
// subtag unknown, more than a language, script and region, or not in its
// canonical form, which the refusal names.
func checkLanguage(field, tag string) error {
	c, ok := langtag.Canonical(tag)
	switch {
	case !ok:
		return problem.Invalid("materials.unknown_language", field, fmt.Sprintf("%q is no language tag with known subtags: a language "+
			"with an optional script and region, as ar, ar-AE or zh-Hant.", tag))
	case c != tag:
		return problem.Invalid("materials.unknown_language", field, fmt.Sprintf("%q is written %s.", tag, c))
	}
	return nil
}

// checkDescriptions checks a material's further descriptions beside its
// default one in language: each in a known language, given once and not
// the default's (422 materials.duplicate_language), and each a text. A
// text's length is the contract's and the database's.
func checkDescriptions(language string, ds []Description) error {
	seen := map[string]bool{}
	for i, d := range ds {
		field := fmt.Sprintf("descriptions[%d]", i)
		if err := checkLanguage(field+".language", d.Language); err != nil {
			return err
		}
		switch {
		case d.Language == language:
			return problem.Invalid("materials.duplicate_language", field+".language", d.Language+
				" is the default description's language; the default description is description.")
		case seen[d.Language]:
			return problem.Invalid("materials.duplicate_language", field+".language", "A description in "+d.Language+" is given twice.")
		}
		seen[d.Language] = true
		if err := problem.RequireText(field+".description", d.Text); err != nil {
			return err
		}
	}
	return nil
}

// writeDescriptions writes a material's further descriptions, setting
// the text of one it has, or had and lost, rather than writing it twice.
func writeDescriptions(ctx context.Context, tx pgx.Tx, p reqctx.Principal, material uuid.UUID, ds []Description) error {
	languages, texts := make([]string, len(ds)), make([]string, len(ds))
	for i, d := range ds {
		languages[i], texts[i] = d.Language, d.Text
	}
	if _, err := tx.Exec(ctx, `INSERT INTO materials.material_description (tenant_id, material_id, language, description, created_by)
		SELECT $1, $2, x.language, x.description, $3 FROM unnest($4::text[], $5::text[]) AS x (language, description)
		ON CONFLICT (tenant_id, material_id, language) DO UPDATE SET description = EXCLUDED.description, removed_at = NULL, removed_by = NULL`,
		p.TenantID, material, p.ID, languages, texts); err != nil {
		return fmt.Errorf("materials: descriptions of material %s: %w", material, err)
	}
	return nil
}

// replaceDescriptions replaces a material's further descriptions: those
// no longer given marked removed, then those given written.
func replaceDescriptions(ctx context.Context, tx pgx.Tx, p reqctx.Principal, m Material, ds []Description) error {
	languages := []string{} // never nil: NOT of ANY of a NULL array keeps all
	for _, d := range ds {
		languages = append(languages, d.Language)
	}
	if _, err := tx.Exec(ctx, `UPDATE materials.material_description SET removed_at = now(), removed_by = $3
		WHERE material_id = $1 AND removed_at IS NULL AND NOT language = ANY($2)`, m.ID, languages, p.ID); err != nil {
		return fmt.Errorf("materials: remove descriptions of material %s: %w", m.Number, err)
	}
	return writeDescriptions(ctx, tx, p, m.ID, ds)
}

// descriptionDetail is descriptions as an audit record names them: each
// text by its language.
func descriptionDetail(ds []Description) map[string]string {
	out := make(map[string]string, len(ds))
	for _, d := range ds {
		out[d.Language] = d.Text
	}
	return out
}
