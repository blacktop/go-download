//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package download

import (
	"os"
	"syscall"
)

func openStagingFile(path string, flag int) (*os.File, error) {
	// NONBLOCK prevents a planted FIFO from hanging before the regular-file
	// check; it has no effect on ordinary disk files.
	return os.OpenFile(path, flag|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o644)
}
