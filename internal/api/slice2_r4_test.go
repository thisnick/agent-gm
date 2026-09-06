package api_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/gm"
)

// R-4. `message.sender.id` and `reactions[].participant_id` served Google's
// own participant ID, which section 4.1 forbids outright -- "a caller never
// sees a raw Google conversation ID, message ID or participant ID on a public
// surface" -- and which in a real deployment embeds the owner's Google
// account ADDRESS, restricted by section 12.2 to /v1/accounts and /v1/health.
//
// The same mismatch silently broke every `sender` filter: queries.go matches
// `sender_participant` against `participants.id`, a `part_` ID, while ingest
// wrote the raw source ID there. `sender=me` returned an empty page rather
// than an error, on every listing and on search, which is the worst kind of
// wrong answer because an empty page is a valid one.
//
// Plant: write m.ParticipantID instead of the derived ID in
// store/messages.go and both tests below fail -- the first naming the raw ID,
// the second at "sender=me returned 0". Planted 2026-09-07.
func TestSlice2_R4_NoServedIDIsARawGoogleID(t *testing.T) {
	s := newServer(t)
	defer s.close()

	accountID := s.addAccount("r4@example.test")
	conv := s.seedConversation(accountID, "conv-r4")
	msg := s.seedMessage(accountID, "conv-r4", "msg-r4", s.Clock.Now(), true)
	// A reaction from Google's own participant ID, which is the shape ingest
	// receives and the shape that used to reach a caller verbatim.
	if err := s.Store.ReplaceReactions(context.Background(), conv.ID, msg.ID, "me",
		[]gm.Reaction{{Type: gm.EmojiTypeLike, Emoji: strptr("👍"), ParticipantIDs: []string{"them"}}}); err != nil {
		t.Fatal(err)
	}

	// Every ID field on every read surface, walked generically: a test that
	// named the two known fields would miss the third when it is added.
	for _, path := range []string{
		"/v1/messages",
		"/v1/messages/" + msg.ID,
		"/v1/conversations",
		"/v1/conversations/" + conv.ID,
		"/v1/contacts",
	} {
		t.Run(path, func(t *testing.T) {
			env := s.call("GET", path, nil).ok(t, 200)
			raw, err := json.Marshal(env.Data)
			if err != nil {
				t.Fatal(err)
			}
			var body any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			walkIDs(t, path, body)
		})
	}
}

// walkIDs asserts that every field whose name ends in `_id`, plus `id`
// itself, carries one of section 4.1's declared prefixes.
func walkIDs(t *testing.T, where string, v any) {
	t.Helper()
	prefixes := []string{
		"acct_", "conv_", "msg_", "att_", "react_", "contact_", "part_",
		"op_", "upl_", "auth_", "req_", "pair_",
	}
	switch node := v.(type) {
	case map[string]any:
		for key, child := range node {
			if s, ok := child.(string); ok && s != "" &&
				(key == "id" || strings.HasSuffix(key, "_id")) {
				ok := false
				for _, p := range prefixes {
					if strings.HasPrefix(s, p) {
						ok = true
						break
					}
				}
				if !ok {
					t.Errorf("%s serves %s=%q, which carries no section 4.1 prefix "+
						"-- a raw Google ID on a public surface", where, key, s)
				}
			}
			walkIDs(t, where, child)
		}
	case []any:
		for _, child := range node {
			walkIDs(t, where, child)
		}
	}
}

// The half the DTO fix alone would not have caught: `sender=me` must return
// the messages the owner sent. Section 7.6 says it "is never ambiguous and
// never an error" -- it was instead always empty.
func TestSlice2_R4_SenderFiltersFindWhatTheyJustWrote(t *testing.T) {
	s := newServer(t)
	defer s.close()

	accountID := s.addAccount("r4-sender@example.test")
	s.seedConversation(accountID, "conv-sender")
	for i := 0; i < 3; i++ {
		s.seedMessage(accountID, "conv-sender", "msg-out-"+string(rune('a'+i)),
			s.Clock.Now().Add(time.Duration(i)*time.Second), true)
	}

	all := s.call("GET", "/v1/messages", nil).ok(t, 200)
	total := len(all.items())
	if total == 0 {
		t.Fatal("no messages were seeded")
	}

	mine := s.call("GET", "/v1/messages?sender=me", nil).ok(t, 200)
	if len(mine.items()) == 0 {
		t.Fatalf("sender=me returned 0 of %d outgoing messages; the filter matches "+
			"participants.id and the column held a raw Google ID", total)
	}
	if len(mine.items()) != total {
		t.Errorf("sender=me returned %d of %d outgoing messages", len(mine.items()), total)
	}

	// A sender that is nobody returns an empty page rather than an error --
	// which is why the empty page above was so easy to miss.
	none := s.call("GET", "/v1/messages?sender=%2B12025550199", nil).ok(t, 200)
	if len(none.items()) != 0 {
		t.Errorf("a sender nobody matches returned %d rows", len(none.items()))
	}
}
