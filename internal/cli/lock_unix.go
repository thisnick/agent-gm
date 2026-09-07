//go:build !windows

package cli

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

// LockWait bounds how long `agm` waits for the credentials lock.
//
// The lock is held across a network exchange -- the whole point of taking it
// before the refresh token is spent is that the rotation and the write-back
// are one operation -- so a slow or hanging server makes a second invocation
// wait for as long as the first one takes. An UNBOUNDED wait turns that into
// a hang with no output, which is the worst of the available answers: the
// operator learns nothing and has nothing to act on. Bounded, they get exit 9
// naming the lock, and nothing has been spent.
//
// Ten seconds is longer than any healthy exchange and shorter than a person's
// patience.
// It is a var rather than a const only so that a test can shorten it: a test
// that really waited ten seconds to prove a deadline exists would add ten
// seconds to every CI run to assert a number.
var LockWait = 10 * time.Second

// lockRetry is how often the non-blocking attempt is repeated. It is short
// enough that an ordinary handover is imperceptible.
const lockRetry = 25 * time.Millisecond

// lockFile takes an exclusive advisory lock on an already-open file, waiting
// at most LockWait.
//
// Two `agm` invocations rewriting the credentials file at once would otherwise
// interleave a rotated refresh token with a stale one, and a rotated token is
// single-use: whichever write lost would have lost the credential
// (spec section 11.5).
//
// LOCK_NB in a bounded loop rather than a blocking LOCK_EX: a blocking call
// cannot be given a deadline, and the deadline is the point.
func lockFile(f *os.File) error {
	deadline := time.Now().Add(LockWait)
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
				"if none is running, remove the lock file", LockWait)
		}
		time.Sleep(lockRetry)
	}
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// LockFileForTest exposes lockFile so that a test in this package's external
// test package can hold the lock the way a second `agm` process would. It is
// not called by any production path.
func LockFileForTest(f *os.File) error { return lockFile(f) }
