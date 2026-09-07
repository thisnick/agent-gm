//go:build windows

package cli

import (
	"os"
	"time"
)

const lockSupported = false

// Windows has no flock. The lock file is still created and held open for the
// length of the write, which is what makes the O_CREATE succeed or fail
// visibly; the atomic rename is the part that keeps the file consistent
// either way.
//
// There is therefore nothing to wait for and `wait` is ignored. Two
// concurrent `agm` processes can still lose a rotation here; that is a Slice 4
// problem, and LockSupported exists so a test can say so out loud instead of
// passing for the wrong reason.
func lockFile(*os.File, time.Duration) error { return nil }

func unlockFile(*os.File) error { return nil }
