package cli

import (
	"os"
	"time"
)

// The credentials lock, and the part of it that is the same on every platform.
//
// The lock is held **across the network exchange** on purpose: a rotation and
// its write-back are one operation, and a rotated refresh token is
// single-use, so a second `agm` that interleaved would destroy the credential
// (spec section 11.5). That is also why the wait for it needs a deadline —
// see DefaultLockWait.

// DefaultLockWait bounds how long `agm` waits for the credentials lock.
//
// An unbounded wait turns "another agm is busy" into a hang with no output:
// the operator learns nothing and has nothing to act on. Bounded, they get
// exit 9 naming the lock, with nothing spent. Ten seconds is longer than any
// healthy exchange and shorter than a person's patience.
const DefaultLockWait = 10 * time.Second

// LockSupported reports whether this platform has an advisory file lock at
// all. It is false on Windows, where lockFile is a no-op and two concurrent
// `agm` processes can still lose a rotation — a Slice 4 problem, named here
// so that a test can say so rather than silently assert nothing.
const LockSupported = lockSupported

// lockWait is the deadline this store uses, defaulting when unset.
func (s *Store) lockWait() time.Duration {
	if s.LockWait > 0 {
		return s.LockWait
	}
	return DefaultLockWait
}

// LockForTest takes the credentials lock the way a second `agm` process
// would, so that a test can hold it. It is not called by any production path.
//
// It lives here, without a build constraint, because the test that uses it
// has none either: a symbol declared only for one platform is a test package
// that does not compile for the others, which is exactly how this file came
// to exist.
func LockForTest(f *os.File) error { return lockFile(f, DefaultLockWait) }
