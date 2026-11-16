package enablementagent

import (
	"errors"
	"testing"
)

// TestRolesOnlyWholeTokens: an allowed name is taken out of a text as a
// whole token, so a short field name (id) does not hide an invented role
// (ap_id_clerk) it happens to be a part of.
func TestRolesOnlyWholeTokens(t *testing.T) {
	roles := map[string]bool{"payables_clerk": true}
	for name, tc := range map[string]struct {
		prose   string
		allowed []string
		wantErr bool
	}{
		"an invented role around a short field": {"The id belongs to ap_id_clerk.", []string{"id"}, true},
		"the field itself is no role":           {"The id is the invoice's number.", []string{"id"}, false},
		"the field in backticks":                {"The `id` is the invoice's number.", []string{"id"}, false},
		"two fields side by side":               {"id name", []string{"id", "name"}, false},
		"a dotted code taken out whole":         {"The code payables.match_required means it.", []string{"payables.match_required"}, false},
		"a real role beside the field":          {"payables_clerk reads the id.", []string{"id"}, false},
		"an empty allowed name changes nothing": {"ap_clerk", []string{""}, true},
	} {
		t.Run(name, func(t *testing.T) {
			err := rolesOnly(tc.prose, roles, tc.allowed)
			if got := errors.Is(err, ErrRole); got != tc.wantErr {
				t.Errorf("rolesOnly(%q, %v): got error %v, want a role refusal %v", tc.prose, tc.allowed, err, tc.wantErr)
			}
		})
	}
}
