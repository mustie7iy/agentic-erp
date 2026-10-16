package authn

import "testing"

// TestRefusePrivate: in production a provider is fetched only from a
// public address, checked on the address dialled (ADR-0031).
func TestRefusePrivate(t *testing.T) {
	for _, c := range []struct {
		address string
		allowed bool
	}{
		{"93.184.216.34:443", true},
		{"[2606:2800:220:1:248:1893:25c8:1946]:443", true},
		{"127.0.0.1:443", false},
		{"[::1]:443", false},
		{"10.1.2.3:443", false},
		{"192.168.0.1:443", false},
		{"169.254.169.254:80", false},
		{"[fe80::1]:443", false},
		{"0.0.0.0:443", false},
		{"224.0.0.1:443", false},
		{"no port", false},
		{"not-an-ip:443", false},
	} {
		t.Run(c.address, func(t *testing.T) {
			err := refusePrivate("tcp", c.address, nil)
			if got := err == nil; got != c.allowed {
				t.Errorf("allowed: got %v (%v), want %v", got, err, c.allowed)
			}
		})
	}
}
