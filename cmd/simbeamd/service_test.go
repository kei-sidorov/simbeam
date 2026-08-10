package main

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestServicePlist(t *testing.T) {
	p := servicePlist("/Users/a & b/.local/bin/simbeamd", "/Users/a & b/Library/Logs/simbeamd.log")
	if err := xml.Unmarshal([]byte(p), new(any)); err != nil {
		t.Fatalf("plist is not well-formed XML: %v", err)
	}
	for _, want := range []string{
		"<string>/Users/a &amp; b/.local/bin/simbeamd</string>",
		"<string>serve</string>",
		"<key>KeepAlive</key><true/>",
		"/Users/a &amp; b/.local/bin:/opt/homebrew/bin",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist missing %q:\n%s", want, p)
		}
	}
}
