package workflow

import (
	"testing"
)

// TestReloadingIsClientOnly: the credentials refuse to serve, clone as
// themselves (keeping the reload) and leave the server name to the
// configuration.
func TestReloadingIsClientOnly(t *testing.T) {
	r := reloading{}
	if _, _, err := r.ServerHandshake(nil); err == nil {
		t.Error("ServerHandshake: got served, want refused")
	}
	if _, ok := r.Clone().(reloading); !ok {
		t.Errorf("Clone: got %T, want reloading", r.Clone())
	}
	if err := r.OverrideServerName("x"); err != nil {
		t.Errorf("OverrideServerName: got %v, want nil", err)
	}
	if got := r.Info().SecurityProtocol; got != "tls" {
		t.Errorf("Info: got %q, want tls", got)
	}
}
