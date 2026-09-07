package api_test

import (
	"context"
	"net/url"
	"sync"
	"testing"

	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/gm/fake"
	"github.com/thisnick/agent-gm/internal/store"
)

// Section 16 Slice 2 test 15:
//
//	"Emoji canonicalisation: `add_reaction` with `❤` then
//	 `DELETE …/reactions/❤️` removes it, and the reverse also works; both
//	 derive the same `react_` ID. A second, different emoji from the same
//	 person **replaces** the first (one row, `SWITCH` sent). A reaction whose
//	 type has no unicode serves `{"emoji": null, "type": "emotify"}`.
//	 `DELETE /v1/reactions/{react_id}` removes by ID; somebody else's is
//	 `unsupported_capability` with `not_my_reaction`."
//
// Canonicalisation is the kind of rule that looks like tidiness and is
// actually correctness. `❤` (U+2764) and `❤️` (U+2764 U+FE0F) are the same
// reaction to every human who sees them and two different strings to a
// computer. Without canonicalisation, adding one and removing the other
// leaves the reaction in place -- the agent believes it undid something it
// did not -- and the same reaction acquires two `react_` IDs, so a client
// holding one of them cannot address the other.

// reactionRecorder wraps a fake and records the ReactionAction each call
// carried, which is the one thing the fake does not keep and the one thing
// "SWITCH sent" needs. It is a wrapper rather than a change to the fake
// because a fake that recorded everything anybody ever wanted would stop
// being a model of the backend.
type reactionRecorder struct {
	*fake.Backend
	mu      sync.Mutex
	actions []gm.ReactionAction
}

func (r *reactionRecorder) React(ctx context.Context, msgID, emoji string, action gm.ReactionAction) error {
	r.mu.Lock()
	r.actions = append(r.actions, action)
	r.mu.Unlock()
	return r.Backend.React(ctx, msgID, emoji, action)
}

func (r *reactionRecorder) Actions() []gm.ReactionAction {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]gm.ReactionAction(nil), r.actions...)
}

const (
	heartBare = "❤"          // ❤
	heartVS16 = "❤️"         // ❤️
	thumbsUp  = "\U0001F44D" // 👍
)

func TestSlice2_15_EmojiCanonicalisationBothWays(t *testing.T) {
	for _, order := range []struct {
		name        string
		add, remove string
	}{
		{"add bare, remove with the variation selector", heartBare, heartVS16},
		{"add with the variation selector, remove bare", heartVS16, heartBare},
	} {
		t.Run(order.name, func(t *testing.T) {
			s := newServer(t)
			accountID, recorder := s.addAccountRecordingReactions(addressA)
			s.seedConversation(accountID, "conv-a")
			msg := s.seedMessage(accountID, "conv-a", "m0001", s.Clock.Now(), false)

			added := s.call("POST", "/v1/messages/"+msg.ID+"/reactions", map[string]any{
				"emoji":             order.add,
			}).ok(t, 200)
			if added.Data["operation"] == nil {
				t.Fatal("adding a reaction produced no operation")
			}

			reactions := s.reactionsOf(t, msg.ID)
			if len(reactions) != 1 {
				t.Fatalf("after one add there are %d reaction rows, want 1", len(reactions))
			}
			addedID := reactions[0].ID

			// Both spellings derive the SAME react_ ID, because the ID is
			// derived from the canonical emoji rather than from what the
			// caller typed.
			wantID := store.ReactionID(msg.ID, reactions[0].ParticipantID, heartVS16)
			if addedID != wantID {
				t.Errorf("react_ ID is %s, want the canonical derivation %s", addedID, wantID)
			}

			// Removing by the OTHER spelling addresses the same row.
			s.call("DELETE",
				"/v1/messages/"+msg.ID+"/reactions/"+url.PathEscape(order.remove),
				map[string]any{}).ok(t, 200)

			if left := s.reactionsOf(t, msg.ID); len(left) != 0 {
				t.Fatalf("removing %q did not remove the reaction added as %q: %d rows left",
					order.remove, order.add, len(left))
			}
			if got := recorder.Actions(); len(got) != 2 || got[1] != gm.ReactionActionRemove {
				t.Errorf("the backend saw %v, want an add then a REMOVE", got)
			}
		})
	}
}

// TestSlice2_15_ASecondEmojiReplacesTheFirst is the one-reaction-per-person
// rule (D23). Google's picker is single-select, so an add over an existing
// reaction is a SWITCH, not a second entry -- and a server that sent ADD
// would leave the owner with two reactions in Agent GM and one on the phone.
func TestSlice2_15_ASecondEmojiReplacesTheFirst(t *testing.T) {
	s := newServer(t)
	accountID, recorder := s.addAccountRecordingReactions(addressA)
	s.seedConversation(accountID, "conv-a")
	msg := s.seedMessage(accountID, "conv-a", "m0001", s.Clock.Now(), false)

	s.call("POST", "/v1/messages/"+msg.ID+"/reactions", map[string]any{
		"emoji": heartBare,
	}).ok(t, 200)
	s.call("POST", "/v1/messages/"+msg.ID+"/reactions", map[string]any{
		"emoji": thumbsUp,
	}).ok(t, 200)

	rows := s.reactionsOf(t, msg.ID)
	if len(rows) != 1 {
		t.Fatalf("the same person has %d reactions on one message, want 1", len(rows))
	}
	if rows[0].Emoji != thumbsUp {
		t.Errorf("the surviving reaction is %q, want the second one %q", rows[0].Emoji, thumbsUp)
	}

	actions := recorder.Actions()
	if len(actions) != 2 {
		t.Fatalf("the backend was called %d times, want 2", len(actions))
	}
	if actions[0] != gm.ReactionActionAdd {
		t.Errorf("the first call was %v, want ADD", actions[0])
	}
	if actions[1] != gm.ReactionActionSwitch {
		t.Errorf("the second call was %v, want SWITCH: an ADD over an existing "+
			"reaction would leave two", actions[1])
	}

	// Adding exactly the reaction the owner already has is a no-op: changed
	// false, operation null, and the backend is not called a third time.
	repeat := s.call("POST", "/v1/messages/"+msg.ID+"/reactions", map[string]any{
		"emoji": thumbsUp,
	}).ok(t, 200)
	if repeat.Data["changed"] != false || repeat.Data["operation"] != nil {
		t.Errorf("re-adding the same reaction answered changed=%v operation=%v, "+
			"want false and null", repeat.Data["changed"], repeat.Data["operation"])
	}
	if len(recorder.Actions()) != 2 {
		t.Errorf("re-adding the same reaction called the backend again: %v", recorder.Actions())
	}
}

// TestSlice2_15_AReactionWithNoUnicodeIsServedNotDropped is spec section
// 3.7's consequence. Upstream silently skips a reaction whose EmojiType has
// no unicode of its own; Agent GM serves `{"emoji": null, "type": "emotify"}`
// instead, because dropping it tells a caller that nobody reacted -- a
// different statement, and a false one.
func TestSlice2_15_AReactionWithNoUnicodeIsServedNotDropped(t *testing.T) {
	s := newServer(t)
	accountID := s.addAccount(addressA)
	s.seedConversation(accountID, "conv-a")

	be := s.backend(accountID)
	m := gm.Message{
		SourceID:       "m-emotify",
		ConversationID: "conv-a",
		ParticipantID:  "part-a",
		Text:           "reacted to by an emotify",
		Timestamp:      s.Clock.Now(),
		StatusRaw:      100,
		Kind:           gm.MessageKindMessage,
		DeliveryState:  gm.DeliveryStateReceived,
		Reactions: []gm.Reaction{{
			// No unicode: this is the shape upstream drops.
			Emoji:          nil,
			Type:           gm.EmojiTypeEmotify,
			ParticipantIDs: []string{"part-a"},
		}},
	}
	be.SeedMessage(m)
	if _, err := s.engine(accountID).Ingest().IngestMessage(context.Background(), m, true, false); err != nil {
		t.Fatalf("ingesting: %v", err)
	}

	msgID := store.MessageID(accountID, "conv-a", "m-emotify")
	env := s.call("GET", "/v1/messages/"+msgID, nil).ok(t, 200)
	list, _ := env.Data["reactions"].([]any)
	if len(list) != 1 {
		t.Fatalf("the message serves %d reactions, want 1: a reaction with no unicode "+
			"must be served, not dropped", len(list))
	}
	reaction, _ := list[0].(map[string]any)
	if emoji, present := reaction["emoji"]; !present || emoji != nil {
		t.Errorf("emoji is %v, want null", reaction["emoji"])
	}
	if reaction["type"] != string(gm.EmojiTypeEmotify) {
		t.Errorf("type is %v, want %q", reaction["type"], gm.EmojiTypeEmotify)
	}
}

// TestSlice2_15_RemoveByIDAndSomebodyElsesReaction covers the two remaining
// clauses: removal by `react_` ID, and the refusal for a reaction the owner
// did not send.
func TestSlice2_15_RemoveByIDAndSomebodyElsesReaction(t *testing.T) {
	s := newServer(t)
	accountID := s.addAccount(addressA)
	s.seedConversation(accountID, "conv-a")
	msg := s.seedMessage(accountID, "conv-a", "m0001", s.Clock.Now(), false)

	// The owner's own reaction, removed by ID.
	s.call("POST", "/v1/messages/"+msg.ID+"/reactions", map[string]any{
		"emoji": thumbsUp,
	}).ok(t, 200)
	mine := s.reactionsOf(t, msg.ID)
	if len(mine) != 1 {
		t.Fatalf("want one reaction, have %d", len(mine))
	}
	s.call("DELETE", "/v1/reactions/"+mine[0].ID,
		map[string]any{}).ok(t, 200)
	if left := s.reactionsOf(t, msg.ID); len(left) != 0 {
		t.Fatalf("DELETE /v1/reactions/{id} left %d rows", len(left))
	}

	// Somebody else's, which arrives by ingest rather than by the API.
	be := s.backend(accountID)
	other := gm.Message{
		SourceID:       "m-theirs",
		ConversationID: "conv-a",
		ParticipantID:  "part-a",
		Timestamp:      s.Clock.Now(),
		StatusRaw:      100,
		Kind:           gm.MessageKindMessage,
		DeliveryState:  gm.DeliveryStateReceived,
		Reactions: []gm.Reaction{{
			Emoji: strptr(thumbsUp), Type: gm.EmojiTypeLike,
			ParticipantIDs: []string{"part-a"},
		}},
	}
	be.SeedMessage(other)
	if _, err := s.engine(accountID).Ingest().IngestMessage(context.Background(), other, true, false); err != nil {
		t.Fatalf("ingesting: %v", err)
	}
	theirMsgID := store.MessageID(accountID, "conv-a", "m-theirs")
	theirs := s.reactionsOf(t, theirMsgID)
	if len(theirs) != 1 {
		t.Fatalf("want one foreign reaction, have %d", len(theirs))
	}
	if theirs[0].IsMine {
		t.Fatal("the seeded reaction is marked as the owner's own; the fixture is wrong")
	}

	env := s.call("DELETE", "/v1/reactions/"+theirs[0].ID,
		map[string]any{}).
		refused(t, "unsupported_capability")
	if got := env.detail("reason"); got != "not_my_reaction" {
		t.Errorf("details.reason is %v, want \"not_my_reaction\"", got)
	}
	// The refusal happens BEFORE any operation row exists (spec section 7.8),
	// so a refused action leaves no record that looks like an attempt.
	if n := countRows(t, s.Store,
		"SELECT COUNT(*) FROM operations WHERE kind = 'remove_reaction' AND idempotency_key = ?",
		key("theirs")); n != 0 {
		t.Errorf("a refused removal left %d operation rows", n)
	}
	if left := s.reactionsOf(t, theirMsgID); len(left) != 1 {
		t.Errorf("the refused removal changed the reaction rows: %d left", len(left))
	}
}

func strptr(s string) *string { return &s }
