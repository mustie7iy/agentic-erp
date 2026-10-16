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
		{"100.100.100.200:80", false},
		{"0.1.2.3:443", false},
		{"192.0.0.8:443", false},
		{"198.18.0.1:443", false},
		{"240.0.0.1:443", false},
		{"255.255.255.255:443", false},
		{"[::ffff:127.0.0.1]:443", false},
		{"[::ffff:93.184.216.34]:443", true},
		{"[64:ff9b::a9fe:a9fe]:80", false},
		{"[64:ff9b:1::1]:80", false},
		{"[2002:a9fe:a9fe::1]:80", false},
		{"[2001:0:4136:e378::1]:80", false},
		{"[fc00::1]:443", false},
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
