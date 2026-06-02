package server

import "testing"

func TestKeyUsage(t *testing.T) {
	cases := []struct {
		key   string
		usage uint64
		shift bool
		ok    bool
	}{
		{"a", 4, false, true},
		{"A", 4, true, true},
		{"z", 29, false, true},
		{"1", 30, false, true},
		{"0", 39, false, true},
		{"!", 30, true, true},
		{")", 39, true, true},
		{" ", 44, false, true},
		{"Enter", 40, false, true},
		{"Backspace", 42, false, true},
		{"Tab", 43, false, true},
		{"Escape", 41, false, true},
		{"ArrowUp", 82, false, true},
		{"ArrowDown", 81, false, true},
		{"ArrowLeft", 80, false, true},
		{"ArrowRight", 79, false, true},
		{"-", 45, false, true},
		{"_", 45, true, true},
		{"?", 56, true, true},
		{"F1", 0, false, false}, // unsupported
	}
	for _, c := range cases {
		u, sh, ok := keyUsage(c.key)
		if ok != c.ok || (ok && (u != c.usage || sh != c.shift)) {
			t.Errorf("keyUsage(%q) = (%d,%v,%v), want (%d,%v,%v)", c.key, u, sh, ok, c.usage, c.shift, c.ok)
		}
	}
}
