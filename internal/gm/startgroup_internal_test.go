package gm

import (
	"context"
	"testing"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// recordingCreator captures what was actually sent on each
// GetOrCreateConversation, by value: the request is MUTATED for the retry, so
// keeping the pointer would make both calls look like the second one.
type recordingCreator struct {
	statuses []gmproto.GetOrCreateConversationResponse_Status
	calls    []struct {
		numbers []string
		name    *string
		create  *bool
	}
}

func (r *recordingCreator) GetOrCreateConversation(_ context.Context,
	req *gmproto.GetOrCreateConversationRequest) (*gmproto.GetOrCreateConversationResponse, error) {
	var call struct {
		numbers []string
		name    *string
		create  *bool
	}
	for _, n := range req.GetNumbers() {
		call.numbers = append(call.numbers, n.GetNumber())
	}
	if req.RCSGroupName != nil {
		v := *req.RCSGroupName
		call.name = &v
	}
	if req.CreateRCSGroup != nil {
		v := *req.CreateRCSGroup
		call.create = &v
	}
	r.calls = append(r.calls, call)

	status := gmproto.GetOrCreateConversationResponse_SUCCESS
	if len(r.statuses) > 0 {
		status = r.statuses[0]
		r.statuses = r.statuses[1:]
	}
	return &gmproto.GetOrCreateConversationResponse{
		Status:       status,
		Conversation: &gmproto.Conversation{ConversationID: "conv-fixture"},
	}, nil
}

// D34, and S-3. The group name is NOT sent on the first
// GetOrCreateConversation: it goes on the CreateRCSGroup retry, which is the
// call that actually creates a group.
//
// This is a deliberate divergence from upstream, which sets RCSGroupName on
// the first call (startchat.go:189 at the pin, fixture assertion 21). The
// Slice 2 live gate found that a named start with SMS/MMS recipients FAILS
// there, and the same start without a name succeeds seconds later against the
// same phone and the same numbers.
//
// It needs its own test because the divergence is invisible everywhere else:
// the fake takes a name and a list of numbers, not a request, so putting the
// name back on the first call leaves every package green while reintroducing
// exactly the bug the gate found -- and to a reader diffing this file against
// upstream, putting it back is the obvious "fix".
//
// Plant: set req.RCSGroupName before the first call and this fails at "the
// first call carried the group name". Planted 2026-09-07.
func TestResolveConversationDoesNotNameTheGroupOnTheFirstCall(t *testing.T) {
	ctx := context.Background()

	t.Run("a plain start asks the question and nothing more", func(t *testing.T) {
		rec := &recordingCreator{}
		if _, err := resolveConversation(ctx, rec, []string{"+15555550101", "+15555550102"},
			"agent-gm test"); err != nil {
			t.Fatalf("resolving: %v", err)
		}
		if len(rec.calls) != 1 {
			t.Fatalf("upstream was called %d times, want 1: SUCCESS needs no retry", len(rec.calls))
		}
		if rec.calls[0].name != nil {
			t.Errorf("the first call carried the group name (%q). A name is an RCS "+
				"group concept and Google refuses it on the call that is still "+
				"deciding whether this is an RCS group at all -- which is a live "+
				"failure no fake reproduces (D34)", *rec.calls[0].name)
		}
		if rec.calls[0].create != nil {
			t.Errorf("the first call carried CreateRCSGroup=%v; the first call asks "+
				"the question, it does not answer it", *rec.calls[0].create)
		}
		if len(rec.calls[0].numbers) != 2 {
			t.Errorf("the first call carried %d numbers, want 2", len(rec.calls[0].numbers))
		}
	})

	t.Run("CREATE_RCS retries with the name and the flag", func(t *testing.T) {
		rec := &recordingCreator{statuses: []gmproto.GetOrCreateConversationResponse_Status{
			gmproto.GetOrCreateConversationResponse_CREATE_RCS,
			gmproto.GetOrCreateConversationResponse_SUCCESS,
		}}
		res, err := resolveConversation(ctx, rec, []string{"+15555550101", "+15555550102"},
			"agent-gm test")
		if err != nil {
			t.Fatalf("resolving: %v", err)
		}
		if len(rec.calls) != 2 {
			t.Fatalf("upstream was called %d times, want 2: CREATE_RCS retries once", len(rec.calls))
		}
		if rec.calls[0].name != nil {
			t.Errorf("the first call carried the group name (%q) even here", *rec.calls[0].name)
		}
		if rec.calls[1].name == nil || *rec.calls[1].name != "agent-gm test" {
			t.Errorf("the retry carried name %v, want \"agent-gm test\": the retry IS "+
				"the group creation, so the name is dropped entirely if it does not "+
				"go here", rec.calls[1].name)
		}
		if rec.calls[1].create == nil || !*rec.calls[1].create {
			t.Errorf("the retry carried CreateRCSGroup %v, want true", rec.calls[1].create)
		}
		if res.Conversation == nil {
			t.Error("the retry's conversation was dropped")
		}
	})

	t.Run("an unnamed start still sends a non-nil empty name on the retry", func(t *testing.T) {
		rec := &recordingCreator{statuses: []gmproto.GetOrCreateConversationResponse_Status{
			gmproto.GetOrCreateConversationResponse_CREATE_RCS,
			gmproto.GetOrCreateConversationResponse_SUCCESS,
		}}
		if _, err := resolveConversation(ctx, rec,
			[]string{"+15555550101", "+15555550102"}, ""); err != nil {
			t.Fatalf("resolving: %v", err)
		}
		// Upstream sends ptr.Ptr("") rather than nil here, and so does Agent
		// GM: a nil name on the CREATE_RCS retry is the one thing the retry
		// is documented not to accept.
		if rec.calls[1].name == nil {
			t.Error("the retry sent a nil group name; upstream sends a non-nil empty " +
				"string (startchat.go:215-216, fixture assertion 21)")
		} else if *rec.calls[1].name != "" {
			t.Errorf("the retry sent name %q for an unnamed start", *rec.calls[1].name)
		}
	})
}
