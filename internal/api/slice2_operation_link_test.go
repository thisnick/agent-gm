package api_test

import (
	"testing"
)

// U-1. `messages.operation_id` was declared in section 4.2 and served in
// every message DTO, and nothing ever wrote it: it was null on every row
// Agent GM had ever stored. A caller asking "which send produced this
// message" got a definite "there was no send" for a message its own send had
// produced -- the reverse of operations.message_id, which the echo
// correlation had been writing all along.
//
// Plant: drop the LinkMessageOperation call in correlateEcho and this fails
// at "the echoed message names no operation". Planted 2026-09-07.
func TestSlice2_TheEchoedMessageNamesTheSendThatProducedIt(t *testing.T) {
	s := newServer(t)
	defer s.close()

	accountID := s.addAccount("oplink@example.test")
	conv := s.seedConversation(accountID, "conv-oplink")

	sent := s.call("POST", "/v1/conversations/"+conv.ID+"/messages", map[string]any{
		"text":              "which send was this?",
	}).ok(t, 200)
	op, _ := sent.Data["operation"].(map[string]any)
	if op == nil {
		t.Fatal("the send returned no operation")
	}
	operationID, _ := op["id"].(string)
	if operationID == "" {
		t.Fatal("the operation has no ID")
	}

	drainEcho(t, s, accountID)

	messages := s.call("GET", "/v1/messages", nil).ok(t, 200)
	found := false
	for _, item := range messages.items() {
		m, _ := item.(map[string]any)
		if text, _ := m["text"].(string); text != "which send was this?" {
			continue
		}
		found = true
		got, present := m["operation_id"]
		if !present {
			t.Fatal("the message DTO omits operation_id entirely")
		}
		if got == nil {
			t.Fatalf("the echoed message names no operation, but %s produced it: a "+
				"field that is declared, served and never written is worse than an "+
				"absent one, because null reads as a definite answer", operationID)
		}
		if got != operationID {
			t.Errorf("the message names operation %v, want %s", got, operationID)
		}
	}
	if !found {
		t.Fatal("the sent message is not in the list")
	}
}
