package api_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/clock"
)

// Section 16 Slice 2 test 1:
//
//	"A populated fake-backed database survives a restart with identical list
//	 output, identical IDs and identical ordering, including across two
//	 messages with the same millisecond timestamp."
//
// The last clause is the whole test. Identical output after a restart is easy
// when every row has a distinct timestamp, because any ordering that sorts by
// time gets the same answer twice. It stops being easy the moment two rows
// share a millisecond, which Google produces routinely in a burst: an
// ordering that fell back to rowid, to insertion order, or to whatever SQLite
// felt like would be stable within one process and would quietly reshuffle
// after a restart -- and a client paging through the list would see a message
// twice, or never.
//
// The fix is that the cursor and the ORDER BY both encode `(sent_at_ms, id)`
// rather than an offset. This test is what proves it, so it deliberately puts
// FOUR messages on two shared milliseconds.
func TestSlice2_1_ListingSurvivesARestartWithIdenticalOrdering(t *testing.T) {
	dir := t.TempDir()
	clk := clock.NewFake()

	first := openServer(t, dir, clk)
	accountA := first.addAccount(addressA)
	accountB := first.addAccount(addressB)

	// Two accounts, because a cross-account listing is where an ordering that
	// happens to work per account can still shuffle (spec section 13.1).
	convA := first.seedConversation(accountA, "conv-a")
	convB := first.seedConversation(accountB, "conv-b")

	base := clk.Now().Truncate(time.Millisecond)
	// Two pairs of messages, each pair on ONE millisecond.
	first.seedMessage(accountA, "conv-a", "m0001", base, false)
	first.seedMessage(accountA, "conv-a", "m0002", base, false)
	first.seedMessage(accountA, "conv-a", "m0003", base.Add(time.Second), false)
	first.seedMessage(accountB, "conv-b", "m0004", base.Add(time.Second), false)
	first.seedMessage(accountB, "conv-b", "m0005", base.Add(2*time.Second), true)

	before := map[string][]string{
		"messages":      idsOf(t, first.call("GET", "/v1/messages?limit=100", nil).ok(t, 200)),
		"conversations": idsOf(t, first.call("GET", "/v1/conversations?limit=100", nil).ok(t, 200)),
		"contacts":      idsOf(t, first.call("GET", "/v1/contacts?limit=100", nil).ok(t, 200)),
		"convA":         idsOf(t, first.call("GET", "/v1/conversations/"+convA.ID+"/messages?limit=100", nil).ok(t, 200)),
		"convB":         idsOf(t, first.call("GET", "/v1/conversations/"+convB.ID+"/messages?limit=100", nil).ok(t, 200)),
	}
	if len(before["messages"]) != 5 {
		t.Fatalf("seeded 5 messages, listed %d", len(before["messages"]))
	}
	if len(before["convA"]) != 3 {
		t.Fatalf("account a has 3 messages, listed %d", len(before["convA"]))
	}

	// The restart: same directory, same data key, a new process's worth of
	// in-memory state and a new store handle.
	first.HTTP.Close()
	if err := first.Store.Close(); err != nil {
		t.Fatalf("closing the store: %v", err)
	}

	second := openServer(t, dir, clk)
	after := map[string][]string{
		"messages":      idsOf(t, second.call("GET", "/v1/messages?limit=100", nil).ok(t, 200)),
		"conversations": idsOf(t, second.call("GET", "/v1/conversations?limit=100", nil).ok(t, 200)),
		"contacts":      idsOf(t, second.call("GET", "/v1/contacts?limit=100", nil).ok(t, 200)),
		"convA":         idsOf(t, second.call("GET", "/v1/conversations/"+convA.ID+"/messages?limit=100", nil).ok(t, 200)),
		"convB":         idsOf(t, second.call("GET", "/v1/conversations/"+convB.ID+"/messages?limit=100", nil).ok(t, 200)),
	}

	for name, want := range before {
		got := after[name]
		if len(got) != len(want) {
			t.Fatalf("%s: %d rows before the restart, %d after", name, len(want), len(got))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: row %d is %s after the restart and was %s before; "+
					"the ordering is not stable across equal timestamps",
					name, i, got[i], want[i])
			}
		}
	}

	// Paging one row at a time is where an unstable tiebreak shows up as a
	// duplicate or a hole rather than as a reordering, so the walk is done
	// with a page size of one across the two shared milliseconds.
	walked := walkAll(t, second, "/v1/conversations/"+convA.ID+"/messages", 1)
	if len(walked) != len(before["convA"]) {
		t.Fatalf("paging one at a time returned %d rows, want %d: %v",
			len(walked), len(before["convA"]), walked)
	}
	seen := map[string]bool{}
	for i, id := range walked {
		if seen[id] {
			t.Errorf("paging returned %s twice", id)
		}
		seen[id] = true
		if id != before["convA"][i] {
			t.Errorf("paged row %d is %s, want %s", i, id, before["convA"][i])
		}
	}
}

// idsOf pulls the `id` of every row out of a listing's data.items.
func idsOf(t *testing.T, e envelope) []string {
	t.Helper()
	var out []string
	for _, row := range e.items() {
		obj, ok := row.(map[string]any)
		if !ok {
			t.Fatalf("a listing row is %T, want an object", row)
		}
		id, _ := obj["id"].(string)
		out = append(out, id)
	}
	return out
}

// walkAll pages a listing to the end with the given page size, sending the
// SAME query string every time -- which is what a cursor's binding to the
// filter set as written requires (spec section 7.4).
func walkAll(t *testing.T, s *server, path string, limit int) []string {
	t.Helper()
	var out []string
	cursor := ""
	for range 100 {
		url := path + "?limit=" + itoa(limit)
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		env := s.call("GET", url, nil).ok(t, 200)
		out = append(out, idsOf(t, env)...)
		if env.NextCursor == nil || *env.NextCursor == "" {
			return out
		}
		cursor = *env.NextCursor
	}
	t.Fatal("paging did not terminate in 100 pages")
	return nil
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
