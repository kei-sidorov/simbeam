package idb

import "testing"

func TestFreePortReturnsUsablePort(t *testing.T) {
	p, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	if p <= 0 || p > 65535 {
		t.Fatalf("port out of range: %d", p)
	}
}

// TestFreePortMultipleCalls verifies repeated calls each yield a usable port
// without error. It deliberately does NOT assert the two ports differ: the OS
// may legitimately reuse a just-freed ephemeral port, so a distinctness check
// would be flaky.
func TestFreePortMultipleCalls(t *testing.T) {
	a, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	b, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	if a <= 0 || b <= 0 {
		t.Fatalf("got non-usable ports: %d, %d", a, b)
	}
}
