//go:build linux

package mpv

import "golang.org/x/sys/unix"

func flushInput(fd int) {
	_ = unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH)
}
