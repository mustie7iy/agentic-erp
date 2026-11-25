package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestMaterialDescriptionRules (S1a2, migrations 000469 and 000470;
// ADR-0082 As built S1a2): a material's default description is in a
// language of the stored form (a language, an optional script and an
// optional region, in canonical case), 'en' when the insert names none,
// and that language does not change. A further description is of a
// material that exists, in a language of that form other than the
// default's, once per language, its text 1 to 200 characters and not
// blank; it is marked removed with who removed it, or neither; its text
// and mark are updated, its language and material not, and none is
// deleted.
func TestMaterialDescriptionRules(t *testing.T) {
	need(t)
	type scene struct{ tenant, material uuid.UUID }
	const material = `INSERT INTO materials.material (tenant_id, id, number, legal_entity_id, description, base_uom_code, material_type, material_group,
			status, created_by, updated_by) VALUES ($1, $2, 'MA1', $1, 'Bracket', 'EA', 'trading_good', 'B', 'active', $1, $1)`
	create := func(s scene) stmt { return step(material, s.tenant, s.material) }
	createIn := func(s scene, language string) stmt {
		return step(`INSERT INTO materials.material (tenant_id, id, number, legal_entity_id, description, description_language, base_uom_code, material_type,
			material_group, status, created_by, updated_by) VALUES ($1, $2, 'MA1', $1, 'Bracket', $3, 'EA', 'trading_good', 'B', 'active', $1, $1)`,
			s.tenant, s.material, language)
	}
	describe := func(s scene, language, text string) stmt {
		return step(`INSERT INTO materials.material_description (tenant_id, material_id, language, description, created_by) VALUES ($1, $2, $3, $4, $1)`,
			s.tenant, s.material, language, text)
	}
	arabic := func(s scene) []stmt { return []stmt{create(s), describe(s, "ar", "دعامة M10")} }
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"a material in English by default with a description in Arabic", arabic, ""},
		{"a material in Arabic by default", func(s scene) []stmt { return []stmt{createIn(s, "ar"), describe(s, "en", "Bracket")} }, ""},
		{"a default in a language, script and region", func(s scene) []stmt { return []stmt{createIn(s, "zh-Hant-TW")} }, ""},
		{"a default in a region of digits", func(s scene) []stmt { return []stmt{createIn(s, "es-419")} }, ""},
		{"a default in capitals", func(s scene) []stmt { return []stmt{createIn(s, "EN")} }, "material_description_language_check"},
		{"a default with an extension", func(s scene) []stmt { return []stmt{createIn(s, "en-US-x-a")} }, "material_description_language_check"},
		{"a default's language changed", func(s scene) []stmt {
			return []stmt{create(s), step(`UPDATE materials.material SET description_language = 'ar', version = version + 1`)}
		}, "default description language does not change"},
		{"a description of no material", func(s scene) []stmt { return []stmt{describe(s, "ar", "X")} }, "material_description_material_fkey"},
		{"a description in the default's language", func(s scene) []stmt { return []stmt{create(s), describe(s, "en", "Bracket")} },
			"the default description's language has no further description"},
		{"a language twice", func(s scene) []stmt { return append(arabic(s), describe(s, "ar", "Y")) }, "material_description_pkey"},
		{"a language in capitals", func(s scene) []stmt { return []stmt{create(s), describe(s, "AR", "X")} }, "material_description_tag_check"},
		{"a language of a region alone", func(s scene) []stmt { return []stmt{create(s), describe(s, "AE", "X")} }, "material_description_tag_check"},
		{"a blank text", func(s scene) []stmt { return []stmt{create(s), describe(s, "ar", "  ")} }, "material_description_text_check"},
		{"a text of 201 characters", func(s scene) []stmt { return []stmt{create(s), describe(s, "ar", strings.Repeat("د", 201))} },
			"material_description_text_check"},
		{"a text of 200 characters", func(s scene) []stmt { return []stmt{create(s), describe(s, "ar", strings.Repeat("د", 200))} }, ""},
		{"a text changed and the description removed", func(s scene) []stmt {
			return append(arabic(s), step(`UPDATE materials.material_description SET description = 'Y', removed_at = now(), removed_by = tenant_id`))
		}, ""},
		{"a description removed by no one", func(s scene) []stmt {
			return append(arabic(s), step(`UPDATE materials.material_description SET removed_at = now()`))
		}, "material_description_removed_check"},
		{"a language changed", func(s scene) []stmt {
			return append(arabic(s), step(`UPDATE materials.material_description SET language = 'fr'`))
		}, "permission denied"},
		{"a description moved to another material", func(s scene) []stmt {
			return append(arabic(s), step(`UPDATE materials.material_description SET material_id = gen_random_uuid()`))
		}, "permission denied"},
		{"a description deleted", func(s scene) []stmt { return append(arabic(s), step(`DELETE FROM materials.material_description`)) }, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), material: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, c.run(s)...) })
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
	t.Run("a default named by no insert", func(t *testing.T) {
		s := scene{tenant: id(), material: id()}
		var got string
		err := asApp(t, s.tenant, func(tx pgx.Tx) error {
			if err := all(tx, create(s)); err != nil {
				return err
			}
			return tx.QueryRow(context.Background(), `SELECT description_language FROM materials.material WHERE id = $1`, s.material).Scan(&got)
		})
		if err != nil || got != "en" {
			t.Errorf("got %q (%v), want en", got, err)
		}
	})
}

// TestMaterialDescriptionTriggersAsOwner (S1a2, migration 000469): past
// the grants, as the owner, a material's default description language
// stays as created and a further description is never in it, even one
// removed or given back by clearing its mark.
func TestMaterialDescriptionTriggersAsOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	tenant, material := id(), id()
	if _, err := testDB.Exec(ctx, `INSERT INTO materials.material (tenant_id, id, number, legal_entity_id, description, base_uom_code, material_type,
			material_group, status, created_by, updated_by) VALUES ($1, $2, $3, $1, 'Bracket', 'EA', 'trading_good', 'B', 'active', $1, $1)`,
		tenant, material, "MA-"+material.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := testDB.Exec(ctx, `INSERT INTO materials.material_description (tenant_id, material_id, language, description, created_by,
			removed_at, removed_by) VALUES ($1, $2, 'ar', 'دعامة', $1, now(), $1)`, tenant, material); err != nil {
		t.Fatalf("a removed description: got %v, want it written", err)
	}
	for set, want := range map[string]string{
		"UPDATE materials.material SET description_language = 'ar' WHERE id = $1":                                                "material_language_fixed",
		"UPDATE materials.material_description SET language = 'en' WHERE material_id = $1":                                       "material_description_not_default",
		"UPDATE materials.material_description SET removed_at = NULL, removed_by = NULL, language = 'en' WHERE material_id = $1": "material_description_not_default",
	} {
		t.Run(set, func(t *testing.T) {
			_, err := testDB.Exec(ctx, set, material)
			failsOn(t, err, want)
		})
	}
}

// TestMaterialDescriptionLanguageBackfilled: 000469 gives every material
// created before it the default description language en, and 000470
// validates its check.
func TestMaterialDescriptionLanguageBackfilled(t *testing.T) {
	conn := emptyDatabase(t)
	migrateBefore(t, conn, "000469_material_descriptions")
	tenant, material := id(), id()
	inTenant(t, conn, tenant, func(tx pgx.Tx) error {
		return all(tx, step(`INSERT INTO materials.material (tenant_id, id, number, legal_entity_id, description, base_uom_code, material_type,
			material_group, status, created_by, updated_by) VALUES ($1, $2, 'MA1', $1, 'Bracket', 'EA', 'trading_good', 'B', 'active', $1, $1)`,
			tenant, material))
	})
	if _, err := Migrate(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	var language string
	var validated bool
	if err := conn.QueryRow(context.Background(), `SELECT m.description_language, c.convalidated FROM materials.material m,
			pg_constraint c WHERE m.id = $1 AND c.conname = 'material_description_language_check'`, material).Scan(&language, &validated); err != nil {
		t.Fatal(err)
	}
	if language != "en" {
		t.Errorf("language: got %q, want en", language)
	}
	if !validated {
		t.Error("the check: got not validated, want 000470 to validate it")
	}
}
