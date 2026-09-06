//go:build windows

package cli

import "os"

// Windows has no flock. The lock file is still created and held open for the
// length of the write, which is what makes the O_CREATE succeed or fail
// visibly; the atomic rename below is the part that keeps the file
// consistent either way.
func lockFile(*os.File) error { return nil }

func unlockFile(*os.File) error { return nil }
