//go:build !windows

package cli

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock on an already-open file. Two
// `agm` invocations rewriting the credentials file at once would otherwise
// interleave a rotated refresh token with a stale one, and a rotated token is
// single-use: whichever write lost would have lost the credential
// (spec section 11.5).
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
