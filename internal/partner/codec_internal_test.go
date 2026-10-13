package partner

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// TestCodecErrors: a malformed date or nullable string fails its decode
// with the cause still matchable.
func TestCodecErrors(t *testing.T) {
	t.Run("date", func(t *testing.T) {
		var d Date
		err := json.Unmarshal([]byte(`"13/01/2026"`), &d)
		var parse *time.ParseError
		if !errors.As(err, &parse) {
			t.Errorf("got %v, want a *time.ParseError", err)
		}
	})
	t.Run("nullable", func(t *testing.T) {
		var n nullable
		err := n.UnmarshalJSON([]byte(`5`))
		var typ *json.UnmarshalTypeError
		if !errors.As(err, &typ) {
			t.Errorf("got %v, want a *json.UnmarshalTypeError", err)
		}
	})
}
