package store_test

// Spec sections 7.4 and 13.2: cursor signing -- tamper rejection,
// filter-binding rejection, and stability across equal timestamps.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

func testKey(t *testing.T) store.DataKey {
	t.Helper()
	k, err := store.ParseDataKey(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("parsing the test data key: %v", err)
	}
	return k
}

func TestCursorRoundTrips(t *testing.T) {
	key := testKey(t)
	fp := store.FilterFingerprint(map[string][]string{"folder": {"active"}})
	want := store.Cursor{SentAtMS: 1700000000123, ID: "msg_00000000-0000-7000-8000-000000000001"}

	tok, err := store.EncodeCursor(key, store.EndpointConversations, fp, want)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	got, err := store.DecodeCursor(key, store.EndpointConversations, fp, tok)
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got != want {
		t.Errorf("round trip gave %+v, want %+v", got, want)
	}
	if strings.Contains(tok, want.ID) {
		t.Error("the cursor is not opaque: it contains the raw ID")
	}
}

func TestCursorRejectsTampering(t *testing.T) {
	key := testKey(t)
	fp := store.FilterFingerprint(map[string][]string{})
	tok, err := store.EncodeCursor(key, store.EndpointMessages, fp,
		store.Cursor{SentAtMS: 10, ID: "msg_a"})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}

	cases := map[string]string{
		"flipped last byte":  tok[:len(tok)-1] + flip(tok[len(tok)-1]),
		"flipped first byte": flip(tok[0]) + tok[1:],
		"truncated":          tok[:len(tok)-4],
		"payload dropped":    tok[strings.Index(tok, ".")+1:],
		"not a cursor":       "hello",
		"empty":              "",
	}
	for name, bad := range cases {
		if _, err := store.DecodeCursor(key, store.EndpointMessages, fp, bad); !errors.Is(err, store.ErrCursorInvalid) {
			t.Errorf("%s: err = %v, want ErrCursorInvalid", name, err)
		}
	}

	// A cursor signed with a different data key is tampering too.
	other, err := store.ParseDataKey(strings.Repeat("cd", 32))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if _, err := store.DecodeCursor(other, store.EndpointMessages, fp, tok); !errors.Is(err, store.ErrCursorInvalid) {
		t.Errorf("a foreign key verified the cursor: %v", err)
	}
}

func flip(b byte) string {
	if b == 'A' {
		return "B"
	}
	return "A"
}

// The binding is to the query AS WRITTEN, not to a normalised form: a cursor
// issued without `folder` is not valid when replayed with `folder=active`,
// although the two select the same rows (spec section 7.4).
func TestCursorIsBoundToTheFilterSetAsWritten(t *testing.T) {
	key := testKey(t)
	issued := store.FilterFingerprint(map[string][]string{"account_id": {"acct_1"}})
	tok, err := store.EncodeCursor(key, store.EndpointConversations, issued,
		store.Cursor{SentAtMS: 10, ID: "conv_a"})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}

	replays := map[string]map[string][]string{
		"folder added, selecting the same rows": {"account_id": {"acct_1"}, "folder": {"active"}},
		"a filter value changed":                {"account_id": {"acct_2"}},
		"a filter removed":                      {},
		"a repeated value added":                {"account_id": {"acct_1", "acct_2"}},
	}
	for name, params := range replays {
		_, err := store.DecodeCursor(key, store.EndpointConversations,
			store.FilterFingerprint(params), tok)
		if !errors.Is(err, store.ErrCursorFilterMismatch) {
			t.Errorf("%s: err = %v, want ErrCursorFilterMismatch", name, err)
		}
	}

	// A different endpoint with the same filters is a mismatch as well: a
	// conversations cursor must not be replayable against messages.
	if _, err := store.DecodeCursor(key, store.EndpointMessages, issued, tok); !errors.Is(err, store.ErrCursorFilterMismatch) {
		t.Errorf("a cursor crossed endpoints: %v", err)
	}

	// And the honest replay still works, so the strictness is not blanket.
	if _, err := store.DecodeCursor(key, store.EndpointConversations, issued, tok); err != nil {
		t.Errorf("the identical query was refused: %v", err)
	}
}

// A filter mismatch and tampering are distinguishable, so the operator can
// tell a client that changed its query from an attacker.
func TestCursorErrorsAreDistinguishable(t *testing.T) {
	if errors.Is(store.ErrCursorFilterMismatch, store.ErrCursorInvalid) ||
		errors.Is(store.ErrCursorInvalid, store.ErrCursorFilterMismatch) {
		t.Fatal("the two cursor errors are not distinguishable")
	}
}

// Spec sections 5.4 and 16 test 14: pagination across ten equal timestamps
// returns each row exactly once.
func TestPaginationAcrossTenEqualTimestamps(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	key := testKey(t)
	acct := seedAccount(t, st, "owner@example.com")

	convID, err := st.UpsertConversation(ctx, acct, gm.Conversation{
		SourceID: "goog-conv-1", Type: gm.ConversationTypeRCS, SendModeRaw: gm.SendModeAuto,
		LastActivity: time.UnixMilli(1700000000000),
	})
	if err != nil {
		t.Fatalf("seeding conversation: %v", err)
	}

	// Ten messages in the same millisecond, which is common in a burst.
	const same = int64(1700000000000)
	for i := 0; i < 10; i++ {
		if _, err := st.UpsertMessage(ctx, acct, "goog-conv-1", gm.Message{
			SourceID: "goog-msg-" + string(rune('a'+i)), Text: "burst",
			Timestamp: time.UnixMilli(same), StatusRaw: 100,
			Kind: gm.MessageKindMessage, DeliveryState: gm.DeliveryStateReceived,
		}); err != nil {
			t.Fatalf("seeding message %d: %v", i, err)
		}
	}

	fp := store.FilterFingerprint(map[string][]string{"conversation_id": {convID}})
	seen := map[string]int{}
	var cursor *store.Cursor
	for page := 0; page < 20; page++ {
		got, err := st.ListMessages(ctx, store.MessageQuery{
			ConversationID: convID, Limit: 3, Cursor: cursor,
		})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(got) == 0 {
			break
		}
		for _, m := range got {
			seen[m.ID]++
		}
		last := got[len(got)-1]
		tok, err := store.EncodeCursor(key, store.EndpointMessages, fp, last.CursorPosition())
		if err != nil {
			t.Fatalf("encoding page %d cursor: %v", page, err)
		}
		next, err := store.DecodeCursor(key, store.EndpointMessages, fp, tok)
		if err != nil {
			t.Fatalf("decoding page %d cursor: %v", page, err)
		}
		cursor = &next
	}

	if len(seen) != 10 {
		t.Fatalf("paged over %d distinct messages, want 10", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("%s was returned %d times, want exactly once", id, n)
		}
	}
}
