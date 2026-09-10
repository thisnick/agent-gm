package lint_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The live gate runs with -race and a ThreadSanitizer suppression file,
// because the pinned libgm leaves its disconnect handshake unsynchronised and
// reports the same races on every pin (see scripts/race-suppressions.txt for
// the full reasoning). The suppressions buy a live gate that can go green;
// what they must not buy is a live gate that goes green while Agent GM's own
// code races.
//
// Two properties give that guarantee, and both are easy to give up by editing
// one word of a text file that nothing reads at build time:
//
//  1. every entry is `race_top:`, which matches only the frame that performed
//     the access. Plain `race:` matches the symbol ANYWHERE in either stack,
//     so a single `race:` entry on postConnect would also mask a genuine race
//     inside triggerEvent, which runs Agent GM's own event handler;
//  2. every entry names a symbol inside the pinned upstream library, so an
//     access made by github.com/thisnick/agent-gm/... is never suppressed.
//
// So this test reads the file the same way the race detector does and holds
// it to both. It is the deployment-host lesson again: a guard nothing
// exercises is a guard that can be weakened invisibly.
func TestRaceSuppressionsStayNarrow(t *testing.T) {
	const upstream = "go.mau.fi/mautrix-gmessages/"

	path := filepath.Join("..", "..", "scripts", "race-suppressions.txt")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the suppression file: %v", err)
	}

	var entries int
	for i, line := range strings.Split(string(body), "\n") {
		lineNo := i + 1
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		entries++

		kind, symbol, ok := strings.Cut(line, ":")
		if !ok {
			t.Errorf("line %d: %q is not a `<type>:<pattern>` suppression", lineNo, line)
			continue
		}
		if kind != "race_top" {
			t.Errorf("line %d: suppression type is %q, want race_top -- %q matches the symbol "+
				"anywhere in either stack and would mask races underneath it, including in "+
				"Agent GM's own code", lineNo, kind, kind)
		}
		if !strings.HasPrefix(symbol, upstream) {
			t.Errorf("line %d: %q is not in %s -- only accesses made by the pinned upstream "+
				"library may be suppressed, never Agent GM's own", lineNo, symbol, upstream)
		}
	}

	// A file that has quietly become empty would pass every check above while
	// telling the next reader that the races are gone.
	if entries == 0 {
		t.Fatal("the suppression file has no entries: if upstream added the locking, delete " +
			"the GORACE flag from devbox.json's test-live rather than keeping an empty file")
	}
}

// The suppressions are only ever meant to apply to the live gate. The offline
// suite runs against fakes, never reaches the racing upstream paths, and must
// stay strict, so `test` must not carry the flag and `test-live` must.
func TestOnlyTheLiveGateCarriesTheSuppressions(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "devbox.json"))
	if err != nil {
		t.Fatalf("reading devbox.json: %v", err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.Contains(line, "go test") {
			continue
		}
		hasFlag := strings.Contains(line, "race-suppressions.txt")
		isLive := strings.Contains(line, "-tags live")
		switch {
		case isLive && !hasFlag:
			t.Errorf("the live gate lost its suppression file and will fail on the upstream "+
				"races again: %s", strings.TrimSpace(line))
		case !isLive && hasFlag:
			t.Errorf("an offline script carries the live gate's suppressions and has gone "+
				"soft on races: %s", strings.TrimSpace(line))
		}
	}
}
