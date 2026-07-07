package main

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// TestRawModeLogWriterAddsCRLF pins the staircase fix: while the terminal is in
// raw mode (watchKeys → term.MakeRaw disables OPOST), standard-logger lines must
// carry a "\r" before every "\n" so reconnect messages don't drift rightward.
func TestRawModeLogWriterAddsCRLF(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	flags := log.Flags()
	log.SetOutput(&buf) // stand in for the raw-mode TTY (stderr)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })

	restore := installRawModeLogWriter()
	log.Print("signaling connection lost\nreconnecting in 1s")
	restore()

	got := buf.String()
	if !strings.Contains(got, "\r\n") {
		t.Fatalf("expected CRLF line endings while raw, got %q", got)
	}
	if strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
		t.Fatalf("found a bare \\n (staircase) in %q", got)
	}

	// After restore the logger is back to bare LF — the wrapper must not leak.
	buf.Reset()
	log.Print("x\ny")
	if strings.Contains(buf.String(), "\r") {
		t.Fatalf("wrapper leaked past restore: %q", buf.String())
	}
}
