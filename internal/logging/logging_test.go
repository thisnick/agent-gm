package logging_test

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/thisnick/agent-gm/internal/logging"
)

// The Slice 1 live gate finding: libgm's own lines ("Skip count is non-zero
// in postConnect, waiting longer") printed inline with `spike list`'s output.
// The library logger is upstream's voice, it goes to stderr, and it is never
// mixed with the command's result.
//
// Plant: give Library the server logger unchanged (drop the warn floor) and
// this test fails at "an info line from the library reached the log".
// Planted 2026-09-06.
func TestLibraryLoggerIsFlooredAtWarn(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error"} {
		t.Run(level, func(t *testing.T) {
			var buf bytes.Buffer
			opts := logging.Options{Level: level}
			lib := logging.Library(logging.NewTo(&buf, opts), opts)

			lib.Info().Msg("Skip count is non-zero in postConnect, waiting longer")
			if buf.Len() != 0 {
				t.Errorf("an info line from the library reached the log: %s", buf.String())
			}
			if level == "error" {
				// The floor is a floor, not a level: a server logger stricter
				// than warn still governs.
				lib.Warn().Msg("upstream warning")
				if buf.Len() != 0 {
					t.Errorf("a warn line survived AGENT_GM_LOG_LEVEL=error: %s", buf.String())
				}
				return
			}
			lib.Warn().Msg("upstream warning")
			if !strings.Contains(buf.String(), "upstream warning") {
				t.Errorf("a genuine upstream warning was swallowed: %q", buf.String())
			}
			if !strings.Contains(buf.String(), `"component":"libgm"`) {
				t.Errorf("the library line is not tagged as upstream's: %q", buf.String())
			}
		})
	}
}

// A one-shot CLI command silences the library entirely: the owner is reading
// a result, and upstream's commentary is not part of it.
func TestQuietSilencesTheLibraryButNotTheServer(t *testing.T) {
	var buf bytes.Buffer
	opts := logging.Options{Level: "info", Quiet: true}
	server := logging.NewTo(&buf, opts)
	lib := logging.Library(server, opts)

	lib.Error().Msg("upstream error")
	if buf.Len() != 0 {
		t.Errorf("the library logger spoke while quiet: %s", buf.String())
	}
	server.Info().Msg("agent gm's own line")
	if !strings.Contains(buf.String(), "agent gm's own line") {
		t.Error("quiet silenced Agent GM's own logger too")
	}
}

// AGENT_GM_UNSAFE_TRACE=1 is the one way past the floor, and it beats quiet:
// somebody who set it is debugging and asked for everything.
func TestUnsafeTraceReachesTraceAndOverridesQuiet(t *testing.T) {
	var buf bytes.Buffer
	opts := logging.Options{Level: "info", UnsafeTrace: true, Quiet: true}
	lib := logging.Library(logging.NewTo(&buf, opts), opts)
	lib.Trace().Msg("payload")
	if !strings.Contains(buf.String(), "payload") {
		t.Errorf("trace did not reach the log: %q", buf.String())
	}
	if !strings.Contains(buf.String(), `"unsafe_trace":true`) {
		t.Errorf("the line is not stamped unsafe_trace: %q", buf.String())
	}
	if logging.LibraryLevel("info") != zerolog.WarnLevel {
		t.Error("LibraryLevel(info) is not warn")
	}
}

// Neither logger ever writes to stdout. Stdout carries the command's result
// -- a table, or the one JSON value --json promises -- and a log line in it
// is a parse error for anything downstream.
//
// Plant: change Writer to os.Stdout and this test fails at "the logger wrote
// to stdout". Planted 2026-09-06.
func TestLoggersNeverWriteToStdout(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	realStdout := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = realStdout })

	opts := logging.Options{Level: "debug", Format: "text"}
	server := logging.New(opts)
	server.Warn().Msg("a server line")
	lib := logging.Library(server, opts)
	lib.Warn().Msg("a library line")

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var captured bytes.Buffer
	if _, err := captured.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	if captured.Len() != 0 {
		t.Errorf("the logger wrote to stdout: %q", captured.String())
	}
}
