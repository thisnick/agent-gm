package api_test

import "testing"

// R-14. `sender=me` was asserted only against a SEEDED conversation. A
// conversation created through the real `POST /v1/conversations` came back
// from the fake with no `is_me` participant -- unlike every real backend and
// unlike the seeding helper -- so its messages' `sender.id` was a well-formed
// `part_` ID that resolved to no participants row, and `sender=me` returned 0
// against the real server.
//
// A fake whose shape differs from the real backend's in one field tests the
// layer above it in a world that does not exist. This drives the whole path a
// caller drives: start a conversation, send into it, then filter.
//
// Plant: drop the is_me participant from the fake's ResolveConversation and
// this fails at "sender=me returned 0". Planted 2026-09-07.
func TestSlice2_R14_SenderMeWorksOnAConversationTheAPICreated(t *testing.T) {
	s := newServer(t)
	defer s.close()

	accountID := s.addAccount("r14@example.test")

	started := s.call("POST", "/v1/conversations", map[string]any{
		"account_id":        accountID,
		"recipients":        []string{"+12025550166"},
		"client_request_id": key("r14-start"),
	}).ok(t, 200)
	conv, _ := started.Data["conversation"].(map[string]any)
	if conv == nil {
		t.Fatal("the start returned no conversation")
	}
	convID, _ := conv["id"].(string)

	// The conversation the API just created must carry the owner's own
	// participant, or nothing downstream can tell who sent what.
	var sawMe bool
	parts, _ := conv["participants"].([]any)
	for _, p := range parts {
		row, _ := p.(map[string]any)
		if isMe, _ := row["is_me"].(bool); isMe {
			sawMe = true
		}
	}
	if !sawMe {
		t.Error("the created conversation has no is_me participant; every sender=me " +
			"query turns on that field")
	}

	s.call("POST", "/v1/conversations/"+convID+"/messages", map[string]any{
		"text":              "hello",
		"client_request_id": key("r14-send"),
	}).ok(t, 200)
	drainEcho(t, s, accountID)

	all := s.call("GET", "/v1/messages", nil).ok(t, 200)
	if len(all.items()) == 0 {
		t.Fatal("the send produced no message row")
	}
	mine := s.call("GET", "/v1/messages?sender=me", nil).ok(t, 200)
	if len(mine.items()) == 0 {
		t.Fatalf("sender=me returned 0 of %d messages on a conversation the API created",
			len(all.items()))
	}
}
