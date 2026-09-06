// Command agm is Agent GM's command-line client (spec section 11).
//
// It speaks the REST API and nothing else: it never touches SQLite or `libgm`
// directly, so anything the CLI can do an agent can do too, and vice versa
// (spec section 11). That is why it is a separate binary from `agent-gm`,
// which is the server: they share a repository and a vocabulary, not a
// process, and `agm` is routinely run from a machine that is not the server
// at all -- the headless-pairing flow of section 11.4 depends on it.
//
// Every exit code this program can produce is section 11.2's, mapped from
// section 7.2's error codes in `internal/apierr` so that the table has one
// home rather than two.
package main

import (
	"os"

	"github.com/thisnick/agent-gm/internal/cli"
)

// version is stamped at build time with -ldflags "-X main.version=...". It
// defaults to `dev` so a build from a working tree says so rather than
// claiming a release it is not.
var version = "dev"

func main() {
	// cli.Run never calls os.Exit itself, so a test can drive the real
	// command with real argument parsing and read its exit code. This is the
	// one place the process actually ends.
	os.Exit(cli.Run(cli.Env{
		Args:    os.Args[1:],
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Version: version,
	}))
}
