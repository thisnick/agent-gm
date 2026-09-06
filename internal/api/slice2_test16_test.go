package api_test

import (
	"testing"
)

// Section 16 Slice 2 test 16:
//
//	"`PATCH /v1/conversations/{id}` archives, unarchives, pins, unpins and
//	 marks unread; repeating one returns `changed: false` with
//	 `operation: null` and calls the backend **zero** times."
//
// The zero is the point. An idempotent PATCH that re-sent the change anyway
// would be indistinguishable, from the outside, from one that had to -- and
// it would spend a mutation budget, and a round trip to a phone, on nothing.
// Worse, it would make `operation` meaningless: a caller asking "did my
// archive land?" would get an operation row for a request that changed
// nothing, every time.
//
// So the assertion is against the FAKE's call counter, not against the
// response alone. A handler that answered `changed: false` while still
// calling the backend would pass a response-only test.
func TestSlice2_16_PatchConversationIsIdempotentAndCallsTheBackendZeroTimes(t *testing.T) {
	s := newServer(t)
	accountID := s.addAccount(addressA)
	conv := s.seedConversation(accountID, "conv-a")
	be := s.backend(accountID)

	patch := func(name string, body map[string]any) envelope {
		t.Helper()
		full := map[string]any{"client_request_id": key(name)}
		for k, v := range body {
			full[k] = v
		}
		return s.call("PATCH", "/v1/conversations/"+conv.ID, full)
	}

	steps := []struct {
		name string
		body map[string]any
		// want is the conversation state the change should produce.
		check func(t *testing.T, conv map[string]any)
	}{
		{"archive", map[string]any{"folder": "archived"}, func(t *testing.T, c map[string]any) {
			if c["folder"] != "archived" {
				t.Errorf("folder is %v, want archived", c["folder"])
			}
		}},
		{"unarchive", map[string]any{"folder": "active"}, func(t *testing.T, c map[string]any) {
			if c["folder"] != "active" {
				t.Errorf("folder is %v, want active", c["folder"])
			}
		}},
		{"pin", map[string]any{"pinned": true}, func(t *testing.T, c map[string]any) {
			if c["pinned"] != true {
				t.Errorf("pinned is %v, want true", c["pinned"])
			}
		}},
		{"unpin", map[string]any{"pinned": false}, func(t *testing.T, c map[string]any) {
			if c["pinned"] != false {
				t.Errorf("pinned is %v, want false", c["pinned"])
			}
		}},
		{"mark_unread", map[string]any{"unread": true}, func(t *testing.T, c map[string]any) {
			if c["unread"] != true {
				t.Errorf("unread is %v, want true", c["unread"])
			}
		}},
	}

	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			before := be.CallCount("UpdateConversation")

			env := patch(step.name, step.body).ok(t, 200)
			if env.Data["changed"] != true {
				t.Fatalf("first %s answered changed=%v, want true", step.name, env.Data["changed"])
			}
			if env.Data["operation"] == nil {
				t.Fatalf("first %s answered operation:null; a change that happened has an operation",
					step.name)
			}
			conversation, _ := env.Data["conversation"].(map[string]any)
			if conversation == nil {
				t.Fatalf("%s did not return the conversation", step.name)
			}
			step.check(t, conversation)

			afterFirst := be.CallCount("UpdateConversation")
			if afterFirst != before+1 {
				t.Fatalf("first %s called UpdateConversation %d times, want exactly 1",
					step.name, afterFirst-before)
			}

			// The repeat. A NEW idempotency key, so this is a genuinely new
			// request rather than a replay of the first -- which is the case
			// that matters: a replay returning the old operation would be
			// correct for a different reason and would not test this rule.
			repeat := patch(step.name+"-repeat", step.body).ok(t, 200)
			if repeat.Data["changed"] != false {
				t.Errorf("repeating %s answered changed=%v, want false",
					step.name, repeat.Data["changed"])
			}
			if repeat.Data["operation"] != nil {
				t.Errorf("repeating %s answered an operation, want operation:null: %v",
					step.name, repeat.Data["operation"])
			}
			if after := be.CallCount("UpdateConversation"); after != afterFirst {
				t.Errorf("repeating %s called UpdateConversation %d more times, want zero",
					step.name, after-afterFirst)
			}
		})
	}
}

// TestSlice2_16_PatchRefusesAnEmptyChange keeps the route from being a way to
// mint an operation row that says nothing.
func TestSlice2_16_PatchRefusesAnEmptyChange(t *testing.T) {
	s := newServer(t)
	accountID := s.addAccount(addressA)
	conv := s.seedConversation(accountID, "conv-a")

	s.call("PATCH", "/v1/conversations/"+conv.ID,
		map[string]any{"client_request_id": key("empty")}).
		refused(t, "invalid_request")

	// `spam_blocked` is Google's own classification, not something a caller
	// may assert, so offering it would be a route that silently did nothing.
	s.call("PATCH", "/v1/conversations/"+conv.ID,
		map[string]any{"folder": "spam_blocked", "client_request_id": key("spam")}).
		refused(t, "invalid_request")
}
