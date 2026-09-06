package accounts_test

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/accounts"
)

// The `backfill.state` vocabulary is closed, and until this slice it was not
// enumerated in the spec at all -- section 7.5 showed "complete" in an example
// and said nothing about the rest, so an agent reading the block had no way to
// know what else it might see. It is closed now (7.5), and this keeps it so.
//
// The distinction that matters is `pending` against `not_started`: scheduled
// and not started yet, against will not be scheduled at all. It is the same
// distinction section 4.7 drew between `degraded` and `parked`, for the same
// reason -- an agent that reads `pending` for a parked account waits for a
// backfill that is never coming.
//
// Plant: add a sixth state to the code without adding it to section 7.5 and
// this test fails naming it. Planted 2026-09-07.
func TestBackfillStateVocabularyIsClosed(t *testing.T) {
	want := []string{"complete", "not_started", "paused", "pending", "running"}

	got := append([]string(nil), accounts.BackfillStates()...)
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("BackfillStates() = %v, want %v", got, want)
	}

	// Every one of them appears in the specification's own table, so the
	// vocabulary a caller reads about is the vocabulary the server emits.
	spec, err := os.ReadFile("../../plans/AGENT_GM_SPEC.md")
	if err != nil {
		t.Fatalf("reading the spec: %v", err)
	}
	section := backfillStateSection(t, string(spec))
	for _, state := range want {
		if !strings.Contains(section, "`"+state+"`") {
			t.Errorf("the spec's backfill.state table does not name %q", state)
		}
	}
	// And the table's own ROWS name nothing the code cannot produce. Only
	// the first cell of each row is a state; the prose around the table
	// legitimately mentions other fields (`completed_at`) and other
	// vocabularies (`parked`, `signed_out`), and reading those as claimed
	// states is how this check would fail for the wrong reason.
	haveState := map[string]bool{}
	for _, state := range want {
		haveState[state] = true
	}
	row := regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\|")
	rows := 0
	for _, cell := range row.FindAllStringSubmatch(section, -1) {
		// The header row, `| `state` | Meaning |`, is not a state.
		if cell[1] == "state" {
			continue
		}
		rows++
		if !haveState[cell[1]] {
			t.Errorf("the spec's table has a row for %q, which BackfillStates() does not have",
				cell[1])
		}
	}
	if rows != len(want) {
		t.Errorf("the spec's table has %d state rows, the code has %d states", rows, len(want))
	}
}

// backfillStateSection extracts the table so the assertion is about that
// table rather than about the whole document, where every one of these words
// appears somewhere.
func backfillStateSection(t *testing.T, spec string) string {
	t.Helper()
	const marker = "**`backfill.state` is a closed vocabulary**"
	i := strings.Index(spec, marker)
	if i < 0 {
		t.Fatal("the spec has no backfill.state vocabulary section")
	}
	rest := spec[i:]
	if j := strings.Index(rest, "\n\n`config_version_live`"); j > 0 {
		return rest[:j]
	}
	return rest[:min(len(rest), 2000)]
}

// Schedulable is the line between `pending` and `not_started`, and it is
// exactly section 4.7's "reads but does not write" split.
func TestSchedulableMatchesTheWritableStates(t *testing.T) {
	for _, tc := range []struct {
		state accounts.State
		want  bool
	}{
		{accounts.StateConnected, true},
		{accounts.StateDegraded, true},
		{accounts.StatePairing, true},
		{accounts.StateParked, false},
		{accounts.StateSignedOut, false},
		{accounts.StateError, false},
		{accounts.StateAccountChanged, false},
	} {
		if got := accounts.Schedulable(tc.state); got != tc.want {
			t.Errorf("Schedulable(%s) = %v, want %v", tc.state, got, tc.want)
		}
	}
	// Every state is decided: a new one added without a decision here would
	// silently fall to not schedulable.
	for _, s := range accounts.States() {
		_ = accounts.Schedulable(s)
	}
}
