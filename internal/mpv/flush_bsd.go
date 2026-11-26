//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package mpv

import "golang.org/x/sys/unix"

// The BSDs have no TCFLSH; re-applying the current settings with the
// "after flush" variant discards unread input just the same.
func flushInput(fd int) {
	if t, err := unix.IoctlGetTermios(fd, unix.TIOCGETA); err == nil {
		_ = unix.IoctlSetTermios(fd, unix.TIOCSETAF, t)
	}
}
