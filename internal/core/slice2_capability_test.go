package core_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/core"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/gm/fake"
	"github.com/thisnick/agent-gm/internal/store"
)

func (h *harness) operationCount() int {
	h.t.Helper()
	var n int
	if err := h.Store.Reader().QueryRowContext(h.ctx(),
		`SELECT COUNT(*) FROM operations`).Scan(&n); err != nil {
		h.t.Fatalf("counting operations: %v", err)
	}
	return n
}

func reasonOf(t *testing.T, err error) string {
	t.Helper()
	var e *apierr.Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want an apierr.Error", err)
	}
	if e.Code != apierr.CodeUnsupportedCapability {
		t.Fatalf("code = %q, want unsupported_capability", e.Code)
	}
	r, ok := e.Details["reason"].(apierr.Reason)
	if !ok {
		t.Fatalf("details.reason = %#v, want a section 7.8 Reason", e.Details["reason"])
	}
	return r.String()
}

// TestSlice2UnsupportedCapabilityReasons covers spec section 7.8's closed
// vocabulary as it is reachable from core's mutations, and the rule that
// gives the vocabulary its value: **each is emitted BEFORE any operation row
// exists**, so a refused action never leaves a record that looks like an
// attempt. Every case asserts the operation count is unchanged.
func TestSlice2UnsupportedCapabilityReasons(t *testing.T) {
	t.Run("conversation_read_only", func(t *testing.T) {
		h := newHarness(t)
		c := h.seedConversation(convA, false)
		c.ReadOnly = true
		if _, err := h.Account.Ingest().IngestConversation(h.ctx(), c); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		before := h.operationCount()
		_, err := h.Account.SendText(h.ctx(), sendInput(h, "k", "hi"))
		if got := reasonOf(t, err); got != "conversation_read_only" {
			t.Fatalf("reason = %q", got)
		}
		if h.operationCount() != before {
			t.Fatal("a refused action created an operation row")
		}
	})

	t.Run("conversation_deleted", func(t *testing.T) {
		h := newHarness(t)
		c := h.seedConversation(convA, false)
		c.Deleted = true
		if _, err := h.Account.Ingest().IngestConversation(h.ctx(), c); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		before := h.operationCount()
		_, err := h.Account.SendText(h.ctx(), sendInput(h, "k", "hi"))
		if got := reasonOf(t, err); got != "conversation_deleted" {
			t.Fatalf("reason = %q", got)
		}
		if h.operationCount() != before {
			t.Fatal("a refused action created an operation row")
		}
	})

	t.Run("reply_not_supported_on_sms", func(t *testing.T) {
		h := newHarness(t)
		c := h.seedConversation(convA, false)
		c.Type = gm.ConversationTypeSMSMMS
		if _, err := h.Account.Ingest().IngestConversation(h.ctx(), c); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		before := h.operationCount()
		in := sendInput(h, "k", "hi")
		in.ReplyToMessageID = "msg-whatever"
		_, err := h.Account.SendText(h.ctx(), in)
		if got := reasonOf(t, err); got != "reply_not_supported" {
			t.Fatalf("reason = %q -- replies are RCS-only", got)
		}
		if h.operationCount() != before {
			t.Fatal("a refused action created an operation row")
		}
	})

	t.Run("rcs_not_available", func(t *testing.T) {
		h := newHarness(t)
		c := h.seedConversation(convA, false)
		c.Type = gm.ConversationTypeSMSMMS // so force_rcs_eligible is false
		if _, err := h.Account.Ingest().IngestConversation(h.ctx(), c); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		before := h.operationCount()
		in := sendInput(h, "k", "hi")
		in.ForceRCS = true
		_, err := h.Account.SendText(h.ctx(), in)
		if got := reasonOf(t, err); got != "rcs_not_available" {
			t.Fatalf("reason = %q", got)
		}
		if h.operationCount() != before {
			t.Fatal("a refused action created an operation row")
		}
	})

	t.Run("not_my_message", func(t *testing.T) {
		h := newHarness(t)
		h.seedConversation(convA, false)
		incoming := message(convA, 1, h.Clock.Now()) // StatusRaw 100: incoming
		res, err := h.Account.Ingest().IngestMessage(h.ctx(), incoming, true, false)
		if err != nil {
			t.Fatalf("ingest: %v", err)
		}
		before := h.operationCount()
		_, err = h.Account.DeleteMessage(h.ctx(), core.DeleteMessageInput{
			Request:   core.Request{AuthorizationID: "authz-1", Key: "k", Body: []byte(`{}`), Source: "test"},
			MessageID: res.ID,
		})
		if got := reasonOf(t, err); got != "not_my_message" {
			t.Fatalf("reason = %q", got)
		}
		if h.operationCount() != before {
			t.Fatal("a refused action created an operation row")
		}
	})

	t.Run("not_signed_in_is_per_account_not_not_paired", func(t *testing.T) {
		h := newHarness(t)
		h.seedConversation(convA, false)
		if err := h.Store.SetAccountState(h.ctx(), h.Account.ID,
			store.StateSignedOut, store.ReasonCredentials); err != nil {
			t.Fatalf("signing out: %v", err)
		}
		before := h.operationCount()
		_, err := h.Account.SendText(h.ctx(), sendInput(h, "k", "hi"))
		if got := reasonOf(t, err); got != "not_signed_in" {
			t.Fatalf("reason = %q -- an account that exists but cannot write is "+
				"not_signed_in, never the service-level not_paired", got)
		}
		if h.operationCount() != before {
			t.Fatal("a refused action created an operation row")
		}
	})

	t.Run("media_pending", func(t *testing.T) {
		att := store.Attachment{ID: "att_1", DownloadState: store.DownloadStatePending}
		if got := reasonOf(t, core.CheckMediaReady(att, "send")); got != "media_pending" {
			t.Fatalf("reason = %q", got)
		}
		att.DownloadState = store.DownloadStateAvailable
		if err := core.CheckMediaReady(att, "send"); err != nil {
			t.Fatalf("a downloaded attachment was refused: %v", err)
		}
	})
}

// reactionRecorder wraps a fake so a test can see which ReactionAction was
// sent. ADD and SWITCH are different wire calls and D23 turns on the
// difference, so a test that only counted calls would not be testing it.
type reactionRecorder struct {
	*fake.Backend
	actions []gm.ReactionAction
	emojis  []string
}

func (r *reactionRecorder) React(ctx context.Context, msgID, emoji string, action gm.ReactionAction) error {
	r.actions = append(r.actions, action)
	r.emojis = append(r.emojis, emoji)
	return r.Backend.React(ctx, msgID, emoji, action)
}

// TestSlice2ReactionsAreCanonicalisedAndSwitched covers spec section 7.6 and
// D23: emoji canonicalised through EmojiType first; ADD, or SWITCH when the
// owner already has a different one; `operation: null` when the owner already
// has exactly that one; removal by emoji or by react_ ID; somebody else's is
// not_my_reaction; one reaction per person per message.
func TestSlice2ReactionsAreCanonicalisedAndSwitched(t *testing.T) {
	h := newHarness(t)
	rec := &reactionRecorder{Backend: h.Backend}
	h.Account.Backend = rec
	conv := h.seedConversation(convA, false)

	m := message(convA, 1, h.Clock.Now())
	h.Backend.SeedMessage(m)
	res, err := h.Account.Ingest().IngestMessage(h.ctx(), m, true, false)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	react := func(key, emoji string) core.Result {
		t.Helper()
		out, err := h.Account.AddReaction(h.ctx(), core.ReactionInput{
			Request: core.Request{
				AuthorizationID: "authz-1", Key: key,
				Body: []byte(`{"emoji":"` + emoji + `"}`), Source: "test",
			},
			MessageID: res.ID,
			Emoji:     emoji,
		})
		if err != nil {
			t.Fatalf("add reaction %q: %v", emoji, err)
		}
		return out
	}

	// The un-qualified heart. Canonicalisation happens BEFORE the react_ ID is
	// derived, so adding "❤" and removing "❤️" are the same reaction.
	first := react("k1", "❤")
	if !first.HasOperation || first.Operation.Status != store.OpSucceeded {
		t.Fatalf("the first reaction did not succeed: %+v", first.Operation)
	}
	if len(rec.actions) != 1 || rec.actions[0] != gm.ReactionActionAdd {
		t.Fatalf("actions = %v, want one ADD", rec.actions)
	}
	if rec.emojis[0] != "❤️" {
		t.Fatalf("the library was sent %q, want the canonical %q",
			rec.emojis[0], "❤️")
	}
	stored, err := h.Store.ReactionsForMessage(h.ctx(), res.ID)
	if err != nil || len(stored) != 1 {
		t.Fatalf("reactions = %v (%v), want exactly one", stored, err)
	}
	if !stored[0].IsMine {
		t.Fatal("the reaction was not recorded as this account's own")
	}
	// The react_ ID hangs off the derived part_ ID, not off Google's own
	// participant ID: section 4.1's table says "message ID, participant ID",
	// and `part_` is what section 4.1 calls a participant.
	wantID := store.ReactionID(res.ID,
		store.ParticipantID(store.ConversationID(h.Account.ID, conv.SourceID), conv.DefaultOutgoingID),
		"❤️")
	if stored[0].ID != wantID {
		t.Fatalf("react_ ID = %s, want the one derived from the canonical emoji %s",
			stored[0].ID, wantID)
	}

	// Exactly the same one again, written the other way: nothing to do.
	calls := h.Backend.CallCount("React")
	same := react("k2", "❤️")
	if same.HasOperation || same.Changed {
		t.Fatal("re-adding the same reaction created an operation")
	}
	if h.Backend.CallCount("React") != calls {
		t.Fatal("re-adding the same reaction called the backend")
	}

	// A different one from the same person REPLACES the first: one row, SWITCH.
	switched := react("k3", "\U0001F44D")
	if !switched.HasOperation {
		t.Fatal("switching produced no operation")
	}
	if last := rec.actions[len(rec.actions)-1]; last != gm.ReactionActionSwitch {
		t.Fatalf("action = %v, want SWITCH", last)
	}
	stored, _ = h.Store.ReactionsForMessage(h.ctx(), res.ID)
	if len(stored) != 1 {
		t.Fatalf("reactions = %d, want one per person per message", len(stored))
	}

	// Removal by emoji.
	out, err := h.Account.RemoveReaction(h.ctx(), core.ReactionInput{
		Request:   core.Request{AuthorizationID: "authz-1", Key: "k4", Body: []byte(`{}`), Source: "test"},
		MessageID: res.ID,
		Emoji:     "\U0001F44D",
	})
	if err != nil {
		t.Fatalf("remove by emoji: %v", err)
	}
	if !out.HasOperation || out.Operation.Status != store.OpSucceeded {
		t.Fatalf("removal did not succeed: %+v", out.Operation)
	}
	stored, _ = h.Store.ReactionsForMessage(h.ctx(), res.ID)
	if len(stored) != 0 {
		t.Fatalf("reactions = %d after removal, want 0", len(stored))
	}

	// Somebody else's reaction is not_my_reaction, refused before any
	// operation row exists -- and removal by react_ ID reaches it.
	theirs := gm.Reaction{Emoji: strptr("\U0001F602"), Type: gm.EmojiTypeLaugh, ParticipantIDs: []string{"part-a"}}
	if err := h.Store.ReplaceReactions(h.ctx(), store.ConversationID(h.Account.ID, conv.SourceID), res.ID, conv.DefaultOutgoingID, []gm.Reaction{theirs}); err != nil {
		t.Fatalf("seeding their reaction: %v", err)
	}
	theirID := store.ReactionID(res.ID,
		store.ParticipantID(store.ConversationID(h.Account.ID, conv.SourceID), "part-a"),
		"\U0001F602")
	before := h.operationCount()
	_, err = h.Account.RemoveReaction(h.ctx(), core.ReactionInput{
		Request:    core.Request{AuthorizationID: "authz-1", Key: "k5", Body: []byte(`{}`), Source: "test"},
		MessageID:  res.ID,
		ReactionID: theirID,
	})
	if got := reasonOf(t, err); got != "not_my_reaction" {
		t.Fatalf("reason = %q", got)
	}
	if h.operationCount() != before {
		t.Fatal("a refused removal created an operation row")
	}
}

func strptr(s string) *string { return &s }

// TestSlice2PatchConversationIsIdempotent covers spec section 7.7: archive,
// unarchive, pin, unpin and mark-unread; repeating one returns
// `changed: false` with `operation: null` and calls the backend ZERO times.
func TestSlice2PatchConversationIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.seedConversation(convA, false)
	convID := store.ConversationID(h.Account.ID, convA)

	patch := func(key string, p core.ConversationPatch) core.Result {
		t.Helper()
		out, err := h.Account.PatchConversation(h.ctx(), p, core.Request{
			AuthorizationID: "authz-1", Key: key, Body: []byte(`{}`),
			ConversationID: convID, Source: "test",
		})
		if err != nil {
			t.Fatalf("patch %s: %v", key, err)
		}
		return out
	}
	yes, no := true, false

	steps := []struct {
		name  string
		patch core.ConversationPatch
		check func(c store.Conversation) bool
	}{
		{"archive", core.ConversationPatch{Archived: &yes},
			func(c store.Conversation) bool { return c.Folder == gm.FolderArchive.String() }},
		{"unarchive", core.ConversationPatch{Archived: &no},
			func(c store.Conversation) bool { return c.Folder == gm.FolderInbox.String() }},
		{"pin", core.ConversationPatch{Pinned: &yes},
			func(c store.Conversation) bool { return c.Pinned }},
		{"unpin", core.ConversationPatch{Pinned: &no},
			func(c store.Conversation) bool { return !c.Pinned }},
		{"mark_unread", core.ConversationPatch{Unread: &yes},
			func(c store.Conversation) bool { return c.Unread }},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			before := h.Backend.CallCount("UpdateConversation")
			out := patch(s.name+"-1", s.patch)
			if !out.Changed || !out.HasOperation {
				t.Fatalf("%s reported no change", s.name)
			}
			if h.Backend.CallCount("UpdateConversation") != before+1 {
				t.Fatalf("%s did not reach the backend once", s.name)
			}
			if !s.check(h.conversation(convA)) {
				t.Fatalf("%s did not apply locally", s.name)
			}

			// Repeating it.
			after := h.Backend.CallCount("UpdateConversation")
			repeat := patch(s.name+"-2", s.patch)
			if repeat.Changed {
				t.Fatalf("repeating %s reported changed: true", s.name)
			}
			if repeat.HasOperation {
				t.Fatalf("repeating %s created an operation; it must be null", s.name)
			}
			if h.Backend.CallCount("UpdateConversation") != after {
				t.Fatalf("repeating %s called the backend %d extra times",
					s.name, h.Backend.CallCount("UpdateConversation")-after)
			}
		})
	}
}

// TestSlice2BackfillPausesForItsOwnPhoneOnly covers spec section 5.2's pause
// rule: THAT account's backfill pauses while a
// MOBILE_DATABASE_SYNC_STARTED/SYNCING alert from ITS phone is outstanding
// and resumes on that phone's MOBILE_DATABASE_SYNC_COMPLETE. One sleepy phone
// never stalls another account.
func TestSlice2BackfillPausesForItsOwnPhoneOnly(t *testing.T) {
	a := newHarness(t)
	b := attachAccount(t, a, "owner-b@example.test")
	convB := a.seedConversation(convA, false)
	b.seedConversation(convA, false)
	for i := range 3 {
		a.Backend.SeedMessage(message(convA, i, a.Clock.Now().Add(-time.Duration(i)*time.Minute)))
		b.Backend.SeedMessage(message(convA, i, b.Clock.Now().Add(-time.Duration(i)*time.Minute)))
	}

	// A's phone starts resyncing its own database.
	if err := a.Account.HandleEvent(a.ctx(),
		&gm.EventUserAlert{Alert: gm.AlertMobileDatabaseSyncStarted}); err != nil {
		t.Fatalf("sync started: %v", err)
	}
	if !a.Account.BackfillPaused() {
		t.Fatal("account A's backfill was not paused by its own phone")
	}
	if b.Account.BackfillPaused() {
		t.Fatal("account A's sleepy phone paused account B")
	}

	// B backfills to completion regardless.
	if err := b.Account.Backfill(b.ctx(), nil); err != nil {
		t.Fatalf("account B backfill: %v", err)
	}
	if b.accountRow().BackfillCompleteAtMS == 0 {
		t.Fatal("account B's backfill_complete_at_ms was not set")
	}
	if a.accountRow().BackfillCompleteAtMS != 0 {
		t.Fatal("account B's completion marked account A complete; " +
			"backfill_complete_at_ms is per account, never a server_meta key")
	}

	// A's backfill blocks until its own phone says it is done.
	done := make(chan error, 1)
	go func() { done <- a.Account.Backfill(a.ctx(), []gm.Conversation{convB}) }()
	select {
	case err := <-done:
		t.Fatalf("account A's backfill ran while its phone was resyncing (err=%v)", err)
	case <-time.After(50 * time.Millisecond):
	}

	if err := a.Account.HandleEvent(a.ctx(),
		&gm.EventUserAlert{Alert: gm.AlertMobileDatabaseSyncComplete}); err != nil {
		t.Fatalf("sync complete: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("account A backfill: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("account A's backfill did not resume on MOBILE_DATABASE_SYNC_COMPLETE")
	}
	if a.accountRow().BackfillCompleteAtMS == 0 {
		t.Fatal("account A's backfill_complete_at_ms was not set")
	}
}

// TestSlice2BackfillResumesFromRecordedProgress covers spec section 5.2 step
// 6: per-conversation progress is in backfill_state, so a restart resumes
// rather than restarting.
func TestSlice2BackfillResumesFromRecordedProgress(t *testing.T) {
	h := newHarness(t)
	conv := h.seedConversation(convA, false)
	base := h.Clock.Now().Add(-300 * time.Minute)
	for i := range 250 {
		h.Backend.SeedMessage(message(convA, i, base.Add(time.Duration(i)*time.Minute)))
	}
	if err := h.Account.Backfill(h.ctx(), []gm.Conversation{conv}); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	convID := store.ConversationID(h.Account.ID, convA)
	if got := h.countMessages(convID); got != 250 {
		t.Fatalf("rows = %d, want 250", got)
	}
	state, err := h.Store.BackfillState(h.ctx(), convID)
	if err != nil {
		t.Fatalf("backfill state: %v", err)
	}
	if !state.Complete || state.MessagesDone != 250 {
		t.Fatalf("state = %+v, want complete with 250 done", state)
	}
	if state.AccountID != h.Account.ID {
		t.Fatalf("backfill_state.account_id = %q, want %q", state.AccountID, h.Account.ID)
	}

	// A second run resumes -- which for a completed thread means it fetches
	// nothing at all.
	calls := h.Backend.CallCount("ListMessages")
	if err := h.Account.Backfill(h.ctx(), []gm.Conversation{conv}); err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if h.Backend.CallCount("ListMessages") != calls {
		t.Fatalf("a completed thread was re-walked: %d extra FetchMessages calls",
			h.Backend.CallCount("ListMessages")-calls)
	}
}
