//go:build !windows

package cli

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

const lockSupported = true

// lockRetry is how often the non-blocking attempt is repeated. It is short
// enough that an ordinary handover is imperceptible.
const lockRetry = 25 * time.Millisecond

// lockFile takes an exclusive advisory lock on an already-open file, waiting
// at most `wait`.
//
// Two `agm` invocations rewriting the credentials file at once would
// otherwise interleave a rotated refresh token with a stale one, and a
// rotated token is single-use: whichever write lost would have lost the
// credential (spec section 11.5).
//
// LOCK_NB in a bounded loop rather than a blocking LOCK_EX: a blocking call
// cannot be given a deadline, and the deadline is the point.
func lockFile(f *os.File, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if err != syscall.EWOULDBLOCK {
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("another agm is holding it (waited %s); "+
				"if none is running, remove the lock file", wait)
		}
		time.Sleep(lockRetry)
	}
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
