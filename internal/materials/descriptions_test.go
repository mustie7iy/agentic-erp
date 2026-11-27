package materials_test

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// arabic is the bracket's description in Arabic, with a Latin part in
// capitals, which a description keeps as given: no case folded.
const arabic = "دعامة مشغولة M10"

// descriptions are a material's descriptions as "language text" each, in
// the order given.
func descriptions(t *testing.T, m map[string]any) string {
	t.Helper()
	var out []string
	for _, d := range rows(t, m, "descriptions") {
		out = append(out, fmt.Sprintf("%v %v", d["language"], d["description"]))
	}
	return strings.Join(out, "; ")
}

// further are descriptions in Arabic and French, the request's form.
func further(ar, fr string) []any {
	return []any{map[string]any{"language": "fr", "description": fr}, map[string]any{"language": "ar", "description": ar}}
}

// TestDescriptions: a material created with further descriptions has its
// description in English by default and one in each other language,
// Arabic as given; a read in a language it has answers it there (ar-AE
// by ar), in any other its default, naming the language answered, as a
// list does. The create change, its audit and MaterialChanged carry them.
func TestDescriptions(t *testing.T) {
	e := setup(t)
	m := e.active(t, map[string]any{"descriptions": further(arabic, "Équerre usinée M10")})
	id := m["id"].(string)
	fields(t, m, map[string]any{"description": "Machined bracket M10", "description_language": "en", "default_language": "en"})
	if got, want := descriptions(t, m), "en Machined bracket M10; ar "+arabic+"; fr Équerre usinée M10"; got != want {
		t.Errorf("descriptions: got %q, want %q", got, want)
	}
	for language, want := range map[string]string{"ar": "ar " + arabic, "ar-AE": "ar " + arabic, "fr-CA": "fr Équerre usinée M10",
		"en-GB": "en Machined bracket M10", "de": "en Machined bracket M10", "und": "en Machined bracket M10"} {
		t.Run(language, func(t *testing.T) {
			got, _ := e.Must(200, "GET", path+"/"+id+"?language="+language, "", "")
			if g := fmt.Sprintf("%v %v", got["description_language"], got["description"]); g != want {
				t.Errorf("got %q, want %q", g, want)
			}
			if got["default_language"] != "en" {
				t.Errorf("default language: got %v, want en", got["default_language"])
			}
		})
	}
	listed, _ := e.Must(200, "GET", path+"?language=ar", "", "")
	if got := rows(t, listed, "items")[0]["description"]; got != arabic {
		t.Errorf("listed in Arabic: got %q, want %q", got, arabic)
	}
	var stored string
	pgtest.Scan(t, `SELECT description FROM materials.material_description WHERE tenant_id = $1 AND material_id = $2 AND language = 'ar'`,
		[]any{e.Tenant.ID, id}, &stored)
	if stored != arabic {
		t.Errorf("stored: got %q, want %q", stored, arabic)
	}
	c, _ := e.Must(200, "GET", "/v1/material-changes?material_id="+id, "", "")
	req := rows(t, c, "items")[0]["requested"].(map[string]any)
	if got, want := descriptions(t, req), "ar "+arabic+"; fr Équerre usinée M10"; req["description_language"] != "en" || got != want {
		t.Errorf("the create change: got %v %q, want en %q", req["description_language"], got, want)
	}
	audit := e.audits(t, id, "materials.material.create")[0]
	fields(t, audit, map[string]any{"description_language": "en", "descriptions": "map[ar:" + arabic + " fr:Équerre usinée M10]"})
	event := e.events(t, "material_changed", id)[0]
	fields(t, event, map[string]any{"description_language": "en"})
	if got, want := descriptions(t, event), "ar "+arabic+"; fr Équerre usinée M10"; got != want {
		t.Errorf("the event's descriptions: got %q, want %q", got, want)
	}
}

// TestDefaultLanguage: a material described by default in Arabic answers
// Arabic but in English, which it has too; one without further
// descriptions answers its default in any language.
func TestDefaultLanguage(t *testing.T) {
	e := setup(t)
	m := e.create(t, map[string]any{"description": arabic, "description_language": "ar",
		"descriptions": []any{map[string]any{"language": "en", "description": "Machined bracket M10"}}})
	fields(t, m, map[string]any{"description": arabic, "description_language": "ar", "default_language": "ar"})
	for language, want := range map[string]string{"en-US": "en Machined bracket M10", "ar": "ar " + arabic, "fr": "ar " + arabic} {
		t.Run(language, func(t *testing.T) {
			got, _ := e.Must(200, "GET", path+"/"+m["id"].(string)+"?language="+language, "", "")
			if g := fmt.Sprintf("%v %v", got["description_language"], got["description"]); g != want {
				t.Errorf("got %q, want %q", g, want)
			}
		})
	}
	plain := e.create(t, map[string]any{"description": "Copper wire"})
	got, _ := e.Must(200, "GET", path+"/"+plain["id"].(string)+"?language=ar", "", "")
	if g := fmt.Sprintf("%v %v %v", got["description_language"], got["description"], descriptions(t, got)); g != "en Copper wire en Copper wire" {
		t.Errorf("no further descriptions: got %q, want the default alone", g)
	}
}

// TestDescriptionsChanged: a change of the further descriptions waits for
// its approval as other fields do and is audited with the texts before
// and after; approved, it replaces them (a text changed, one added, one
// removed). An empty list removes them all, and a description given back
// takes its new text.
func TestDescriptionsChanged(t *testing.T) {
	e := setup(t)
	id := e.active(t, map[string]any{"descriptions": further(arabic, "Équerre")})["id"].(string)
	zinc := arabic + "، مطلية بالزنك"
	body := mustJSON(t, map[string]any{"descriptions": []any{map[string]any{"language": "ar", "description": zinc},
		map[string]any{"language": "de", "description": "Winkel M10"}}, "reason": "Zinc"})
	c := e.change(t, id, body)
	if got, want := descriptions(t, c["requested"].(map[string]any)), "ar "+zinc+"; de Winkel M10"; got != want {
		t.Errorf("requested: got %q, want %q", got, want)
	}
	waiting, _ := e.Must(200, "GET", path+"/"+id+"?language=ar", "", "")
	fields(t, waiting, map[string]any{"description": arabic})
	audit := e.audits(t, c["id"].(string), "materials.change.request")[0]
	fields(t, audit, map[string]any{"fields": "[descriptions]"})
	fields(t, audit["before"].(map[string]any), map[string]any{"descriptions": "map[ar:" + arabic + " fr:Équerre]"})
	fields(t, audit["after"].(map[string]any), map[string]any{"descriptions": "map[ar:" + zinc + " de:Winkel M10]"})
	e.Approve(t, c["id"].(string))
	got, _ := e.Must(200, "GET", path+"/"+id, "", "")
	if g, want := descriptions(t, got), "en Machined bracket M10; ar "+zinc+"; de Winkel M10"; g != want {
		t.Errorf("approved: got %q, want %q", g, want)
	}
	if g := descriptions(t, e.changed(t, id, `{"descriptions": []}`)); g != "en Machined bracket M10" {
		t.Errorf("removed: got %q, want the default alone", g)
	}
	back := e.changed(t, id, mustJSON(t, map[string]any{"descriptions": []any{map[string]any{"language": "ar", "description": arabic}}}))
	if g, want := descriptions(t, back), "en Machined bracket M10; ar "+arabic; g != want {
		t.Errorf("given back: got %q, want %q", g, want)
	}
	_, tag := e.Must(200, "GET", path+"/"+id, "", "")
	e.Problem(422, "materials.nothing_to_change", "PATCH", path+"/"+id,
		mustJSON(t, map[string]any{"descriptions": []any{map[string]any{"language": "ar", "description": arabic}}}), tag)
}

// TestDescriptionRefusals: a language that is no tag with known subtags,
// or not written canonically, the default's language as a further one, a
// language twice and a blank text are refused, on a creation and on a
// change, naming the field; a read's language of another form is a bad
// request.
func TestDescriptionRefusals(t *testing.T) {
	e := setup(t)
	id := e.active(t, nil)["id"].(string)
	one := func(language, text string) []any {
		return []any{map[string]any{"language": language, "description": text}}
	}
	for name, c := range map[string]struct {
		over   map[string]any
		status int
		code   string
		field  string
	}{
		"an unknown language":          {map[string]any{"descriptions": one("zz", "X")}, 422, "materials.unknown_language", "descriptions[0].language"},
		"a deprecated language":        {map[string]any{"descriptions": one("iw", "X")}, 422, "materials.unknown_language", "descriptions[0].language"},
		"an unknown default language":  {map[string]any{"description_language": "zz"}, 422, "materials.unknown_language", "description_language"},
		"the default's language":       {map[string]any{"descriptions": one("en", "Bracket")}, 422, "materials.duplicate_language", "descriptions[0].language"},
		"another default's language":   {map[string]any{"description_language": "ar", "descriptions": one("ar", "X")}, 422, "materials.duplicate_language", "descriptions[0].language"},
		"a language twice":             {map[string]any{"descriptions": append(one("ar", "X"), one("ar", "Y")...)}, 422, "materials.duplicate_language", "descriptions[1].language"},
		"a blank text":                 {map[string]any{"descriptions": one("ar", "  ")}, 422, "request.validation_failed", "descriptions[0].description"},
		"a language of another form":   {map[string]any{"descriptions": one("EN", "X")}, 400, "request.invalid_body", ""},
		"a default of another form":    {map[string]any{"description_language": "en_US"}, 400, "request.invalid_body", ""},
		"a description of 201 letters": {map[string]any{"descriptions": one("ar", strings.Repeat("د", 201))}, 400, "request.invalid_body", ""},
		"a language with an extension": {map[string]any{"descriptions": one("en-US-x-a", "X")}, 400, "request.invalid_body", ""},
	} {
		t.Run("create "+name, func(t *testing.T) {
			status, got, _ := e.Do("POST", path, e.bracket(t, c.over), "")
			if status != c.status || got["code"] != c.code {
				t.Fatalf("got %d %v, want %d %s", status, got["code"], c.status, c.code)
			}
			if c.field != "" && field(got) != c.field {
				t.Errorf("field: got %v, want %s", field(got), c.field)
			}
		})
	}
	for name, c := range map[string]struct {
		body, code, field string
	}{
		"the default's language": {mustJSON(t, map[string]any{"descriptions": one("en", "Bracket")}), "materials.duplicate_language", "descriptions[0].language"},
		"a language twice": {mustJSON(t, map[string]any{"descriptions": append(one("ar", arabic), one("ar", arabic)...)}), "materials.duplicate_language",
			"descriptions[1].language"},
		"an unknown language":  {mustJSON(t, map[string]any{"descriptions": one("xx-XX", "X")}), "materials.unknown_language", "descriptions[0].language"},
		"the default language": {`{"description_language": "ar"}`, "request.invalid_body", ""},
	} {
		t.Run("change "+name, func(t *testing.T) {
			status, got, _ := e.Do("PATCH", path+"/"+id, c.body, `"2"`)
			want := 422
			if c.code == "request.invalid_body" {
				want = 400
			}
			if status != want || got["code"] != c.code {
				t.Fatalf("got %d %v, want %d %s", status, got["code"], want, c.code)
			}
			if c.field != "" && field(got) != c.field {
				t.Errorf("field: got %v, want %s", field(got), c.field)
			}
		})
	}
	e.Problem(400, "request.invalid_parameter", "GET", path+"/"+id+"?language="+url.QueryEscape("en_US"), "", "")
}
