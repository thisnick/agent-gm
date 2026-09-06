package gm_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/gm"
)

// Slice 1 acceptance test 1: go.mod, internal/gm/pin.go and spec section 3.6
// all name be48a58. The pin-consistency script is the CI job; this is the
// same claim as an ordinary test, so a developer who edits one place alone
// fails `devbox run test` too.
func TestPinAgreesAcrossAllThreePlaces(t *testing.T) {
	goMod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}
	re := regexp.MustCompile(`go\.mau\.fi/mautrix-gmessages v[^\s]*-([0-9a-f]{12})`)
	m := re.FindStringSubmatch(string(goMod))
	if m == nil {
		t.Fatal("go.mod does not pin go.mau.fi/mautrix-gmessages by pseudo-version")
	}
	if !strings.HasPrefix(m[1], gm.PinnedUpstreamCommit) {
		t.Errorf("go.mod names %s, internal/gm/pin.go names %s", m[1], gm.PinnedUpstreamCommit)
	}
	if !strings.HasPrefix(gm.PinnedUpstreamCommitFull, gm.PinnedUpstreamCommit) {
		t.Errorf("PinnedUpstreamCommitFull %s does not begin with PinnedUpstreamCommit %s",
			gm.PinnedUpstreamCommitFull, gm.PinnedUpstreamCommit)
	}
	if !strings.HasPrefix(gm.PinnedUpstreamCommitFull, m[1]) {
		t.Errorf("PinnedUpstreamCommitFull %s does not begin with the go.mod hash %s",
			gm.PinnedUpstreamCommitFull, m[1])
	}

	spec, err := os.ReadFile("../../plans/AGENT_GM_SPEC.md")
	if err != nil {
		t.Fatalf("reading the spec: %v", err)
	}
	specRe := regexp.MustCompile(`(?m)^commit:\s+([0-9a-f]{7,40})\s*$`)
	sm := specRe.FindStringSubmatch(string(spec))
	if sm == nil {
		t.Fatal("spec section 3.6 does not state a commit")
	}
	if sm[1] != gm.PinnedUpstreamCommit {
		t.Errorf("spec section 3.6 names %s, internal/gm/pin.go names %s", sm[1], gm.PinnedUpstreamCommit)
	}

	if gm.PinnedUpstreamModule != "go.mau.fi/mautrix-gmessages" {
		t.Errorf("PinnedUpstreamModule is %q", gm.PinnedUpstreamModule)
	}
}

// The compiled ConfigVersion is a property of this binary, and spec section
// 3.6 states its value. A pin bump that changes it is expected to be urgent
// (D3), so it is asserted here as well as in the fixture-validation job.
func TestCompiledConfigVersionMatchesTheSpec(t *testing.T) {
	got := gm.CompiledConfigVersion()
	want := gm.ConfigVersion{Year: 2026, Month: 9, Day: 2, V1: 4, V2: 6}
	if got != want {
		t.Errorf("compiled ConfigVersion is %s, spec section 3.6 says %s", got, want)
	}
	if got.SameDate(gm.ConfigVersion{Year: 2026, Month: 9, Day: 3}) {
		t.Error("SameDate must compare the day")
	}
	if !got.SameDate(gm.ConfigVersion{Year: 2026, Month: 9, Day: 2, V1: 9, V2: 9}) {
		t.Error("SameDate must ignore V1 and V2: the detection rule is the date diff alone")
	}
}
