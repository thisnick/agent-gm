// Package logging builds Agent GM's two loggers.
//
// There are exactly two, and the split is the point. The **server** logger is
// Agent GM's own: request IDs, operation IDs, state transitions, error codes.
// The **library** logger is the one handed to `libgm`, and it is a different
// thing with a different audience — it is upstream's running commentary on a
// protocol Agent GM does not control ("Skip count is non-zero in postConnect,
// waiting longer"), which an owner reading the output of `agm messages list`
// has no use for and no way to act on.
//
// Two rules hold for both, and they are what this package exists to make
// true rather than to remember:
//
//  1. **Neither ever writes to stdout.** Stdout belongs to the command's
//     result — a table, or the one JSON value `--json` promises (§11.3) — and
//     a log line in it is a parse error for anything downstream. Both loggers
//     take an io.Writer that this package only ever sets to stderr.
//  2. **The library logger is floored at warn** and is never more verbose
//     than the server logger. `libgm` at info narrates the long poll; at
//     trace it base64-logs decrypted payloads (§12.2), which is why trace is
//     reachable only behind AGENT_GM_UNSAFE_TRACE=1.
//
// The Slice 1 live gate is the reason this is a package and not four lines in
// main: upstream's warn-level lines printed inline with `spike list`'s own
// output, and the fix has to be somewhere a test can see it.
package logging

import (
	"io"
	"os"
	"time"

	"github.com/rs/zerolog"
)

// Options are the environment's logging settings (spec section 15.1).
type Options struct {
	// Level is AGENT_GM_LOG_LEVEL: debug, info, warn or error.
	Level string
	// Format is AGENT_GM_LOG_FORMAT: json or text.
	Format string
	// UnsafeTrace is AGENT_GM_UNSAFE_TRACE=1. It permits libgm's trace
	// logging, which base64-logs decrypted payloads.
	UnsafeTrace bool
	// Quiet silences the library logger entirely. One-shot CLI commands set
	// it so upstream's commentary never lands in a terminal the owner is
	// reading a result from; the server never does.
	Quiet bool
}

// Level parses a level name, defaulting to info.
func Level(name string) zerolog.Level {
	switch name {
	case "debug":
		return zerolog.DebugLevel
	case "warn":
		return zerolog.WarnLevel
	case "error":
		return zerolog.ErrorLevel
	default:
		return zerolog.InfoLevel
	}
}

// Writer is where both loggers write. It is stderr, always: stdout carries
// the command's result and nothing else.
func Writer(format string) io.Writer {
	return writerTo(os.Stderr, format)
}

func writerTo(dst io.Writer, format string) io.Writer {
	if format == "text" {
		return zerolog.ConsoleWriter{Out: dst, TimeFormat: time.RFC3339}
	}
	return dst
}

// New builds the server logger on stderr.
func New(o Options) zerolog.Logger {
	return NewTo(os.Stderr, o)
}

// NewTo is New with the destination injected, so a test can read what was
// written. Production always passes stderr.
func NewTo(dst io.Writer, o Options) zerolog.Logger {
	l := zerolog.New(writerTo(dst, o.Format)).Level(Level(o.Level)).With().Timestamp().Logger()
	if o.UnsafeTrace {
		l = l.Level(zerolog.TraceLevel).With().Bool("unsafe_trace", true).Logger()
	}
	return l
}

// Library derives the logger handed to libgm from the server logger.
//
// It is tagged `component=libgm` so a reader can tell upstream's voice from
// Agent GM's, and it is floored at warn: upstream's info-level narration of
// the long poll is not a diagnosis of anything an owner can act on, and it
// was what leaked into a command's output at the Slice 1 live gate. Debug or
// info on the server logger does not lower it; only AGENT_GM_UNSAFE_TRACE=1
// does, and then all the way to trace, which is the documented (and audited)
// unsafe case.
func Library(server zerolog.Logger, o Options) zerolog.Logger {
	if o.Quiet && !o.UnsafeTrace {
		return zerolog.Nop()
	}
	l := server.With().Str("component", "libgm").Logger()
	if o.UnsafeTrace {
		return l.Level(zerolog.TraceLevel)
	}
	return l.Level(LibraryLevel(o.Level))
}

// LibraryLevel is the level the library logger runs at for a given
// AGENT_GM_LOG_LEVEL. It is never below warn.
func LibraryLevel(name string) zerolog.Level {
	if l := Level(name); l > zerolog.WarnLevel {
		return l
	}
	return zerolog.WarnLevel
}
