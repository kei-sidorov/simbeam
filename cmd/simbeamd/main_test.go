package main

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
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

// TestSaneRestoreTermiosHealsInheritedRaw pins the second half of the staircase
// fix: even when runRemote inherits a tty that a prior process left raw (OPOST /
// ICANON / ECHO cleared), the state we restore on exit must re-enable the cooked
// flags — otherwise a kill -9'd predecessor would wedge the shell we hand back to,
// and the *next* run's banner (and the shell prompt) would staircase again.
func TestSaneRestoreTermiosHealsInheritedRaw(t *testing.T) {
	var rawInherited unix.Termios // zero value: all flags cleared == fully raw
	sane := saneRestoreTermios(rawInherited)

	cases := []struct {
		name string
		flag uint64
		got  uint64
	}{
		{"OPOST", unix.OPOST, uint64(sane.Oflag)},
		{"ONLCR", unix.ONLCR, uint64(sane.Oflag)},
		{"ICANON", unix.ICANON, uint64(sane.Lflag)},
		{"ECHO", unix.ECHO, uint64(sane.Lflag)},
		{"ISIG", unix.ISIG, uint64(sane.Lflag)},
		{"ICRNL", unix.ICRNL, uint64(sane.Iflag)},
	}
	for _, c := range cases {
		if c.got&c.flag == 0 {
			t.Errorf("restore state missing %s: cooked handoff not guaranteed", c.name)
		}
	}
}
