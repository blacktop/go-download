//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package download

import (
	"fmt"
	"os"
)

// flockSupported reports whether this platform enforces the cross-process
// staging lock (Discard uses it to order close vs. remove).
const flockSupported = false

// lockStaging is a no-op on platforms without BSD flock semantics (Windows,
// Plan 9, js/wasm, WASI, AIX, Solaris): the standard library exposes no
// portable primitive and the package takes no dependencies. Same-process
// downloads are still serialized by the destination path lock.
func lockStaging(f *os.File) error {
	return fmt.Errorf("%w: not available on this platform", errFlockUnsupported)
}
