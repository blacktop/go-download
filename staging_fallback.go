//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || windows)

package download

import "os"

func openStagingFile(path string, flag int) (*os.File, error) {
	// Without a no-follow flag, opening existing files must not create or
	// truncate a link target. openStaging validates the descriptor before use.
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if os.IsNotExist(err) && flag&os.O_CREATE != 0 {
		return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	}
	return f, err
}
