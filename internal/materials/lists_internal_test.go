package materials

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
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

// TestNumberWithoutCompanyDay: a material is not numbered in a company
// whose time zone, in the configuration document read for its creation,
// the binary cannot load: the document is read once, by the creation, and
// number fails with the cause, naming the company.
func TestNumberWithoutCompanyDay(t *testing.T) {
	e := &Endpoints{}
	doc := &config.Config{Companies: []config.Company{{Code: "9999", TimeZone: "Nowhere/Lost"}}}
	_, err := e.number(context.Background(), tenancy.Entity{Code: "9999"}, doc)
	if err == nil || !strings.Contains(err.Error(), "materials: today in company 9999") {
		t.Errorf("got %v, want the company day's error naming company 9999", err)
	}
}
