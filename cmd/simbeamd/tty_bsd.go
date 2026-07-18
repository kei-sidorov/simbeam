//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import "golang.org/x/sys/unix"

// termios get/set ioctl requests on BSD-family kernels (incl. macOS). The Linux
// kernel names these differently (TCGETS/TCSETS, see tty_linux.go), so they live
// in build-tagged files to keep main.go — which also cross-compiles for the Linux
// demo daemon — platform-neutral.
const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)
