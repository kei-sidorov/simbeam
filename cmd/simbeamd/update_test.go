package main

import "testing"

func TestIsBrewPath(t *testing.T) {
	for path, want := range map[string]bool{
		"/opt/homebrew/Caskroom/simbeamd/0.21.0/simbeamd":                true,
		"/opt/homebrew/Cellar/simbeam-control/0.6.0/bin/simbeam-control": true,
		"/Users/x/.local/bin/simbeamd":                                   false,
		"/usr/local/bin/simbeamd":                                        false,
	} {
		if got := isBrewPath(path); got != want {
			t.Errorf("isBrewPath(%q) = %v, want %v", path, got, want)
		}
	}
}
