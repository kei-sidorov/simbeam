//go:build linux

package main

import "golang.org/x/sys/unix"

// termios get/set ioctl requests on Linux (TCGETS/TCSETS). The BSD/macOS names
// differ (TIOCGETA/TIOCSETA, see tty_bsd.go). The interactive raw-mode path never
// runs on the Linux demo daemon (it has no controlling TTY), but main.go must
// still compile for linux/amd64 + linux/arm64 in the release build.
const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
)
