//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package download

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// flockSupported reports whether this platform enforces the cross-process
// staging lock (Discard uses it to order close vs. remove).
const flockSupported = true

// lockStaging places a non-blocking advisory flock on the open staging file
// so a second process cannot co-write the same .part (the in-process path
// lock only serializes one process). The lock is released automatically when
// the file descriptor is closed — the descriptor must therefore stay open
// through installation. After locking, the descriptor is verified against the
// current pathname so an opener paused across another process's install fails.
// Advisory only: NFS honors it on v4 mounts.
func lockStaging(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err := classifyFlockErr(err, f.Name()); err != nil {
		return err
	}

	opened, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat locked staging file %s: %w", f.Name(), err)
	}
	current, err := os.Stat(f.Name())
	if os.IsNotExist(err) {
		return fmt.Errorf("%w: staging path changed while acquiring lock", ErrLocked)
	}
	if err != nil {
		return fmt.Errorf("stat staging path %s after lock: %w", f.Name(), err)
	}
	if !os.SameFile(opened, current) {
		return fmt.Errorf("%w: staging path changed while acquiring lock", ErrLocked)
	}
	return nil
}

// classifyFlockErr maps a raw flock result to the package contract:
// contention becomes ErrLocked, filesystems that cannot flock (some network
// mounts) become errFlockUnsupported (callers proceed unprotected but say
// so), and anything else fails closed.
func classifyFlockErr(err error, name string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN):
		return ErrLocked
	case errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EINVAL):
		return fmt.Errorf("%w: flock: %w", errFlockUnsupported, err)
	default:
		return fmt.Errorf("flock %s: %w", name, err)
	}
}
