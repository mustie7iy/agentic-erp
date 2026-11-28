package materials

import (
	"context"
	"errors"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
)

// noSettings is a configuration that cannot be read.
type noSettings struct{}

var errNoSettings = errors.New("configuration unavailable")

func (noSettings) Document(context.Context) (*config.Config, int32, error) {
	return nil, 0, errNoSettings
}

// TestListsUnreadable: a material request whose configuration cannot be
// read fails with the cause, on a create (its one read, settings) and on
// a change that sets the group, and a change that leaves the group does
// not read it.
func TestListsUnreadable(t *testing.T) {
	e := &Endpoints{Settings: noSettings{}}
	if _, err := e.settings(context.Background()); !errors.Is(err, errNoSettings) {
		t.Errorf("create: got %v, want the configuration's error", err)
	}
	if err := e.changedGroup(context.Background(), []string{"material_group"}, "BRACKETS"); !errors.Is(err, errNoSettings) {
		t.Errorf("a change of the group: got %v, want the configuration's error", err)
	}
	if err := e.changedGroup(context.Background(), []string{"description"}, "BRACKETS"); err != nil {
		t.Errorf("a change of the description: got %v, want no read", err)
	}
}
