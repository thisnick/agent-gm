package main

import "testing"

func TestBackgroundDiagnosticRejectsUnsafeAccountBeforeOpeningSession(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--account", "../outside"},
		{"--account", "/absolute"},
		{"--account", "valid", "unexpected"},
	} {
		if got := spikeBackgroundOnce(args); got != exitUsage {
			t.Errorf("args %q: got %d, want usage error", args, got)
		}
	}
}
